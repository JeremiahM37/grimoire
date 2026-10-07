package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// `grimoire bank import-git REPO` seeds a coding agent's bank with the
// repository's history: one document per commit, so the bank can answer
// "why is it like this" from the commits that made it so.
//
// A commit is immutable, so its sha is a perfect document id: a re-run skips
// every commit the bank already holds and retains only the new ones, and a
// commit retained again by --force changes nothing (the server re-extracts
// only chunks whose hash changed). Merges are skipped — their content is the
// commits they merge.

type gitCommit struct {
	SHA, Author, Email, Date, Subject, Body string
	Files                                   []string // "M\tpath" lines
}

// repoBankName derives the per-repo bank id from a repository's top-level
// directory name, folded into the characters a bank id allows.
func repoBankName(top string) string {
	name := strings.ToLower(filepath.Base(top))
	name = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-._")
	if name == "" {
		name = "repo"
	}
	if len(name) > 64-len("coding-agent:") {
		name = name[:64-len("coding-agent:")]
	}
	return "coding-agent:" + name
}

func runGit(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// gitLog reads the last n non-merge commits, newest first, with the paths
// each one changed.
func gitLog(repo string, n int) ([]gitCommit, error) {
	out, err := runGit(repo, "log", "-n", strconv.Itoa(n), "--no-merges", "--no-color", "--name-status",
		"--format=%x1e%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1d")
	if err != nil {
		return nil, err
	}
	var commits []gitCommit
	for _, rec := range strings.Split(out, "\x1e") {
		head, files, ok := strings.Cut(rec, "\x1d")
		if !ok {
			continue
		}
		parts := strings.SplitN(head, "\x1f", 6)
		if len(parts) != 6 {
			continue
		}
		c := gitCommit{SHA: parts[0], Author: parts[1], Email: parts[2], Date: parts[3],
			Subject: parts[4], Body: strings.TrimSpace(parts[5])}
		for _, line := range strings.Split(files, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				c.Files = append(c.Files, line)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// commitContent renders one commit as the document a bank retains.
func commitContent(repo string, c gitCommit, maxFiles int, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Commit %s in the %s repository, by %s on %s.\n\n", c.SHA[:min(12, len(c.SHA))], repo, c.Author, c.Date)
	b.WriteString(c.Subject)
	b.WriteString("\n")
	if c.Body != "" {
		b.WriteString("\n" + c.Body + "\n")
	}
	if len(c.Files) > 0 {
		b.WriteString("\nChanged files:\n")
		for i, f := range c.Files {
			if i == maxFiles {
				fmt.Fprintf(&b, "… and %d more\n", len(c.Files)-maxFiles)
				break
			}
			b.WriteString(strings.ReplaceAll(f, "\t", " ") + "\n")
		}
	}
	if diff != "" {
		b.WriteString("\nDiff:\n" + diff)
		if !strings.HasSuffix(diff, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// capDiff keeps at most limit bytes of a diff, cut at a line boundary, and
// says how much was left out.
func capDiff(diff string, limit int) string {
	if limit <= 0 || len(diff) <= limit {
		return diff
	}
	cut := diff[:limit]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i+1]
	}
	return cut + fmt.Sprintf("[diff truncated: %d of %d bytes shown]\n", len(cut), len(diff))
}

// existingDocs lists the document ids a bank already holds; a missing bank
// holds none.
func existingDocs(c *bankClient, bank string) (map[string]bool, bool, error) {
	have := map[string]bool{}
	for offset := 0; ; offset += 1000 {
		var page struct {
			Items []struct {
				ID string `json:"document_id"`
			} `json:"items"`
			Total int `json:"total"`
		}
		err := c.do("GET", bankPath(bank, "documents")+"?limit=1000&offset="+strconv.Itoa(offset), nil, &page)
		var ae *apiError
		if errors.As(err, &ae) && ae.status == 404 && ae.json {
			return have, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		for _, d := range page.Items {
			have[d.ID] = true
		}
		if len(page.Items) == 0 || offset+len(page.Items) >= page.Total {
			return have, true, nil
		}
	}
}

func bankImportGit(c *bankClient, f *bankFlags) error {
	repo := "."
	if len(f.pos) > 0 {
		repo = f.pos[0]
	}
	top, err := runGit(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	top = strings.TrimSpace(top)
	repoName := filepath.Base(top)
	bank := f.str("--bank", repoBankName(top))
	limit, err := f.int("--limit", 300)
	if err != nil {
		return err
	}
	maxDiff, err := f.int("--max-diff-bytes", 16000)
	if err != nil {
		return err
	}
	maxFiles, err := f.int("--max-files", 200)
	if err != nil {
		return err
	}
	batch, err := f.int("--batch", 10)
	if err != nil || batch < 1 {
		batch = 10
	}
	commits, err := gitLog(top, limit)
	if err != nil {
		return err
	}
	have, exists, err := existingDocs(c, bank)
	if err != nil {
		return err
	}
	var todo []gitCommit
	for _, cm := range commits {
		if f.on["--force"] || !have["git:"+cm.SHA] {
			todo = append(todo, cm)
		}
	}
	fmt.Printf("%s: %d commits read, %d already in %s, %d to retain\n", repoName, len(commits),
		len(commits)-len(todo), bank, len(todo))
	if f.on["--dry-run"] || len(todo) == 0 {
		return nil
	}
	if !exists {
		// A new per-repo bank starts from the coding-agent template, so its
		// retain mission asks for decisions and conventions, not chatter.
		body := map[string]any{"bank_id": bank, "name": repoName}
		if tpl, err := findTemplate(c, "coding-agent"); err == nil {
			for k, v := range tpl.Manifest.Bank.fields() {
				if k != "name" {
					body[k] = v
				}
			}
		}
		var ae *apiError
		if err := c.do("POST", "/api/banks", body, nil); err != nil && !(errors.As(err, &ae) && ae.status == 409) {
			return err
		}
	}
	items := make([]retainItem, 0, batch)
	done, facts, tokens := 0, 0, 0
	flush := func() error {
		if len(items) == 0 {
			return nil
		}
		res, err := retainBatch(c, bank, items, f.str("--mode", ""), f.on["--async"])
		if err != nil {
			return err
		}
		done += len(items)
		for _, d := range res.Documents {
			facts += d.FactsAdded
		}
		tokens += res.Usage.Total
		fmt.Printf("  %d/%d commits retained\n", done, len(todo))
		items = items[:0]
		return nil
	}
	for _, cm := range todo {
		diff := ""
		if f.on["--diffs"] {
			raw, err := runGit(top, "show", "--format=", "--no-color", "--no-ext-diff", cm.SHA)
			if err != nil {
				return err
			}
			diff = capDiff(raw, maxDiff)
		}
		items = append(items, retainItem{
			Content:    commitContent(repoName, cm, maxFiles, diff),
			DocumentID: "git:" + cm.SHA,
			Timestamp:  cm.Date,
			Context:    "git commit in " + repoName,
			Tags:       []string{"source:git"},
			Metadata: map[string]string{"source": "git", "repo": repoName, "commit": cm.SHA,
				"short_sha": cm.SHA[:min(12, len(cm.SHA))], "author": cm.Author, "author_email": cm.Email,
				"authored_at": cm.Date, "subject": cm.Subject},
		})
		if len(items) == batch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	fmt.Printf("retained %d commits into %s: %d new facts", done, bank, facts)
	if tokens > 0 {
		fmt.Printf(", %d model tokens", tokens)
	}
	fmt.Printf("\nask it: grimoire bank recall %s \"why did we …\"\n", bank)
	return nil
}
