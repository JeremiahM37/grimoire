package main

import (
	"os"
	"os/exec"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// `grimoire edit` opens a note in the user's editor on the real vault file.
//
// The editor is the only thing that writes the file here, so afterwards the
// note is re-indexed the same way the other write commands do it: Upsert reads
// it back from disk. Sealed notes are refused, because the bytes on disk are
// ciphertext and an editor would happily save a broken copy of them.

// editorArgv picks the editor: $VISUAL, then $EDITOR, then vi. The variable may
// carry arguments (EDITOR="code --wait"), so it is split on whitespace.
func editorArgv() []string {
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if v := strings.Fields(os.Getenv(k)); len(v) > 0 {
			return v
		}
	}
	return []string{"vi"}
}

// resolveNote finds the note a user named: a vault path, a path without its
// .md suffix, a title, or the slug of a title. It returns the note's vault
// path, or false when nothing matches.
func resolveNote(e *env, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	candidates := []string{ref}
	if !strings.HasSuffix(ref, ".md") {
		candidates = append(candidates, ref+".md")
	}
	candidates = append(candidates, vault.Slugify(ref)+".md")
	for _, c := range candidates {
		p, err := e.vault.SafePath(c)
		if err != nil {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			if rel, err := e.vault.RelOf(p); err == nil {
				return rel, true
			}
		}
	}
	var rel string
	if err := e.index.DB.QueryRow(
		"SELECT path FROM notes WHERE lower(title)=lower(?) ORDER BY path LIMIT 1", ref,
	).Scan(&rel); err == nil {
		return rel, true
	}
	return "", false
}

// editNote runs the editor on one note, then re-indexes it.
func editNote(e *env, rel string) int {
	note, err := e.vault.Read(rel)
	if err != nil {
		return fail("%v", err)
	}
	if vault.IsEncrypted(note.Body) {
		return fail("%s is sealed, so it cannot be edited on disk; open it through the app", rel)
	}
	p, err := e.vault.SafePath(rel)
	if err != nil {
		return fail("%v", err)
	}
	argv := editorArgv()
	cmd := exec.Command(argv[0], append(argv[1:], p)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := cmd.Run()
	// Re-index even when the editor exits non-zero: many editors write the
	// file and then complain, and the index should follow what is on disk.
	if _, err := e.index.Upsert(rel); err != nil {
		return fail("%v", err)
	}
	if runErr != nil {
		return fail("editor %q failed: %v", argv[0], runErr)
	}
	return 0
}

func cmdEdit(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	if len(args) == 0 {
		rel := e.dailyDir + "/" + vault.Now().Format("2006-01-02") + ".md"
		if _, err := e.vault.Read(rel); err != nil {
			return fail("today's daily note does not exist yet; create it with: grimoire daily")
		}
		return editNote(e, rel)
	}
	ref := strings.Join(args, " ")
	rel, ok := resolveNote(e, ref)
	if !ok {
		return fail("no note %q; create it with: grimoire new %q", ref, ref)
	}
	return editNote(e, rel)
}

// withoutFlag drops every occurrence of a boolean flag from args.
func withoutFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a != name {
			out = append(out, a)
		}
	}
	return out
}
