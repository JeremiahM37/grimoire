package vault

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Walking a vault that reaches outside itself.
//
// filepath.WalkDir never descends into a directory symlink, so a vault could
// only ever contain what lived under its own root. That rules out the obvious
// way to bring an existing directory of markdown — an agent's memory store, a
// repo's docs/, a synced folder — into one searchable vault without copying it,
// and a copy is the one arrangement guaranteed to drift.
//
// Following symlinks is opt-in (GRIMOIRE_FOLLOW_SYMLINKS) and off by default,
// because it converts "anyone who can write a file into the vault" into
// "anyone who can choose what the server reads": a vault fed by a sync client
// or a shared directory would otherwise let a planted symlink pull /etc into
// an API that may be reachable on a network. Enable it when you control what
// is in the vault.

// FollowSymlinks reports whether directory symlinks are traversed. It is a
// deployment decision, so it is read from the environment where the vault is
// constructed rather than threaded through every caller.
func followSymlinks() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GRIMOIRE_FOLLOW_SYMLINKS"))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// WalkTree is filepath.WalkDir over a vault, optionally descending into
// directory symlinks.
//
// Paths handed to fn are always *logical*: they read as if the linked
// directory were a real subdirectory of the vault, so a note's identity is its
// vault path and does not leak where the bytes happen to live.
func WalkTree(root string, follow bool, fn fs.WalkDirFunc) error {
	return walkTree(root, root, root, follow, map[string]bool{root: true}, fn)
}

func walkTree(top, logicalRoot, realRoot string, follow bool, seen map[string]bool, fn fs.WalkDirFunc) error {
	return filepath.WalkDir(realRoot, func(p string, d fs.DirEntry, err error) error {
		logical := p
		if realRoot != logicalRoot {
			rel, relErr := filepath.Rel(realRoot, p)
			if relErr != nil {
				return nil
			}
			logical = filepath.Join(logicalRoot, rel)
		}
		if err != nil || d == nil {
			return fn(logical, d, err)
		}
		// A symlink to a FILE already behaves: WalkDir reports it as a
		// non-directory entry and the caller's own filters decide. Only
		// directories need the extra step.
		if follow && d.Type()&fs.ModeSymlink != 0 {
			target, resolveErr := filepath.EvalSymlinks(p)
			if resolveErr != nil {
				return nil // a broken link is not an error worth aborting on
			}
			info, statErr := os.Stat(target)
			if statErr != nil || !info.IsDir() {
				return fn(logical, d, nil)
			}
			if !descendable(top, target, seen) {
				return nil
			}
			seen[target] = true
			return walkTree(top, logical, target, follow, seen, fn)
		}
		return fn(logical, d, err)
	})
}

// descendable rejects the three ways following a link ends badly: walking the
// same directory twice, walking a directory that contains the vault (which
// would re-enter it forever), and walking anything already inside the vault
// (which would index the same note under two paths).
func descendable(top, target string, seen map[string]bool) bool {
	if seen[target] {
		return false
	}
	if target == top || within(top, target) || within(target, top) {
		return false
	}
	return true
}

func within(parent, child string) bool {
	return strings.HasPrefix(child, strings.TrimSuffix(parent, string(filepath.Separator))+string(filepath.Separator))
}
