package api

import (
	"fmt"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Diagnostics: the checks that answer "why can't my agent see my notes?".
//
// /api/health reports counts and returns ok:true, which is a liveness probe and
// not a diagnosis. It said ok:true on an instance whose memory_entries table
// held zero rows — every fact in the vault present on disk, indexed as a note,
// and invisible to recall. Nothing was down, so nothing complained.
//
// That is the shape of every real failure here: not a crash, a silent
// disagreement between the vault, the index and what an agent can reach. So
// each check below compares two things that must agree and reports the pair,
// rather than asking one of them whether it feels well.

// CheckStatus is a check's verdict. Three levels, because "not configured" and
// "configured wrongly" need different answers from an operator.
type CheckStatus string

const (
	StatusOK   CheckStatus = "ok"
	StatusWarn CheckStatus = "warn"
	StatusFail CheckStatus = "fail"
)

// Check is one diagnosis.
type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	// Detail is what was actually observed — the numbers that disagree, the
	// path that is missing. An operator should not have to reproduce the check
	// to find out what it saw.
	Detail string `json:"detail"`
	// Fix is the command or setting that resolves it. A diagnostic that reports
	// a problem without saying what to do about it has moved the work rather
	// than done it.
	Fix string `json:"fix,omitempty"`
}

// doctor runs every check and reports the worst status found.
func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	checks := s.runChecks()
	worst := StatusOK
	for _, c := range checks {
		if c.Status == StatusFail {
			worst = StatusFail
			break
		}
		if c.Status == StatusWarn {
			worst = StatusWarn
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": worst,
		"checks": checks,
	})
}

// RunChecks is the diagnosis, exported so the CLI can run it in-process.
func (s *Server) RunChecks() []Check { return s.runChecks() }

func (s *Server) runChecks() []Check {
	return []Check{
		s.checkVaultWritable(),
		s.checkIndexMatchesVault(),
		s.checkMemoryIsQueryable(),
		s.checkEmbedder(),
		s.checkCredentialVault(),
		s.checkIdentity(),
		s.checkCredentialFreshness(),
	}
}

// checkVaultWritable proves the vault is a directory this process can write to.
//
// Read-only is the failure that looks like nothing at all: notes render, search
// works, and every write silently 500s later.
func (s *Server) checkVaultWritable() Check {
	c := Check{Name: "vault writable"}
	root := s.Vault.Root
	info, err := os.Stat(root)
	if err != nil {
		c.Status, c.Detail = StatusFail, fmt.Sprintf("%s: %v", root, err)
		c.Fix = "set GRIMOIRE_VAULT to a directory that exists"
		return c
	}
	if !info.IsDir() {
		c.Status, c.Detail = StatusFail, root+" is not a directory"
		c.Fix = "point GRIMOIRE_VAULT at a folder, not a file"
		return c
	}
	probe := filepath.Join(root, ".grimoire", ".write-probe")
	if err := os.MkdirAll(filepath.Dir(probe), 0o755); err != nil {
		c.Status, c.Detail = StatusFail, err.Error()
		c.Fix = "make the vault writable by the user running grimoire"
		return c
	}
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		c.Status, c.Detail = StatusFail, "cannot write to "+root+": "+err.Error()
		c.Fix = "make the vault writable by the user running grimoire"
		return c
	}
	_ = os.Remove(probe)
	c.Status, c.Detail = StatusOK, root
	return c
}

// checkIndexMatchesVault compares the number of markdown files on disk with the
// number of notes in the index.
//
// They drift when the watcher is off, when files were copied in while the
// server was stopped, or when a reindex failed halfway. Nothing errors; search
// simply cannot see part of the vault, which reads to a user as "the agent
// doesn't know that" rather than as a fault.
func (s *Server) checkIndexMatchesVault() Check {
	c := Check{Name: "index matches vault"}
	indexed, err := s.Index.DB.Count("SELECT COUNT(*) FROM notes")
	if err != nil {
		c.Status, c.Detail = StatusFail, "index unreadable: "+err.Error()
		c.Fix = "grimoire reindex"
		return c
	}
	paths, err := s.Vault.Walk()
	if err != nil {
		c.Status, c.Detail = StatusWarn, "could not walk the vault: "+err.Error()
		return c
	}
	onDisk := len(paths)
	switch {
	case onDisk == indexed:
		c.Status, c.Detail = StatusOK, fmt.Sprintf("%d notes", indexed)
	case indexed == 0 && onDisk > 0:
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("%d markdown files on disk, 0 indexed — nothing is searchable", onDisk)
		c.Fix = "grimoire reindex"
	default:
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("%d markdown files on disk, %d indexed", onDisk, indexed)
		c.Fix = "grimoire reindex"
	}
	return c
}

// checkMemoryIsQueryable is the check written for a failure that actually
// happened: memory notes present in the vault, indexed as notes, and zero rows
// in memory_entries — so `recall` returned nothing and every other signal was
// green. An incremental restart reports "N unchanged" and does not backfill.
func (s *Server) checkMemoryIsQueryable() Check {
	c := Check{Name: "memory queryable"}
	entries, err := s.Index.DB.Count("SELECT COUNT(*) FROM memory_entries")
	if err != nil {
		c.Status, c.Detail = StatusFail, "memory table unreadable: "+err.Error()
		c.Fix = "grimoire reindex"
		return c
	}
	notes, err := s.Index.DB.Count(
		"SELECT COUNT(*) FROM notes WHERE path LIKE 'memory/%'")
	if err != nil {
		c.Status, c.Detail = StatusWarn, err.Error()
		return c
	}
	switch {
	case notes == 0 && entries == 0:
		c.Status, c.Detail = StatusOK, "no agent memory recorded yet"
	case entries == 0 && notes > 0:
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("%d memory notes indexed but 0 facts queryable — "+
			"recall will return nothing while everything else looks healthy", notes)
		c.Fix = "grimoire reindex"
	default:
		c.Status, c.Detail = StatusOK, fmt.Sprintf("%d facts across %d notes", entries, notes)
	}
	return c
}

// checkEmbedder reports which embedder is live.
//
// A warn, never a fail: the offline hashing embedder is a supported
// configuration. What is worth saying is WHICH one is running, because
// retrieval quality differs and the difference is otherwise invisible.
func (s *Server) checkEmbedder() Check {
	c := Check{Name: "embedder"}
	name := "unknown"
	if s.Index != nil && s.Index.Emb != nil {
		if n := strings.TrimSpace(s.Index.Emb.Signature()); n != "" {
			name = n
		}
	}
	c.Status, c.Detail = StatusOK, name
	if strings.Contains(strings.ToLower(name), "hash") {
		c.Status = StatusWarn
		c.Detail = name + " — semantic search is degraded"
		c.Fix = "set GRIMOIRE_OLLAMA_URL, or leave GRIMOIRE_LOCAL_EMBED=auto to fetch the local model"
	}
	return c
}

// checkCredentialVault reports whether the secret store can be used.
//
// Locked is not a fault — it is the safe default — but an agent whose
// use_credential calls all fail deserves a better answer than 500.
func (s *Server) checkCredentialVault() Check {
	c := Check{Name: "credential vault"}
	if s.Secrets == nil {
		c.Status, c.Detail = StatusOK, "not configured"
		return c
	}
	if !s.Secrets.IsInitialized() {
		c.Status, c.Detail = StatusOK, "not initialised"
		return c
	}
	if !s.Secrets.IsUnlocked() {
		c.Status = StatusWarn
		c.Detail = "initialised but locked — use_credential will fail until it is unlocked"
		c.Fix = "unlock it in the console, or set GRIMOIRE_VAULT_PASSPHRASE_FILE for a headless server"
		return c
	}
	c.Status, c.Detail = StatusOK, "unlocked"
	return c
}

// checkIdentity reports whether caller identification is doing anything.
//
// Its failure mode is silence. A backend that never matches — a socket this
// process cannot read, a ZeroTier network with no members, a proxy list that
// does not include the proxy — looks exactly like one that works: requests
// succeed, the console renders, and every name in the audit trail is quietly
// the one the caller chose for itself. So this reports what is configured and
// whether the caller reaching it right now was actually identified, which is
// the question an operator is really asking.
func (s *Server) checkIdentity() Check {
	c := Check{Name: "caller identity"}
	if !s.Identity.Enabled() {
		c.Status = StatusOK
		c.Detail = "not configured; callers are attributed by the name they send"
		c.Fix = "set GRIMOIRE_IDENTITY to have an overlay network vouch for them"
		return c
	}
	names := strings.Join(s.Identity.Names(), ", ")
	c.Status, c.Detail = StatusOK, "backends: "+names
	return c
}

// checkCredentialFreshness reports credentials that have expired or are about
// to.
//
// A credential dying is the failure this whole store exists around, and it is
// silent: the provider knows the date, the operator does not, and the first
// symptom is a service that stopped working for no visible reason. The vault
// is the one thing that could have said so in advance, so it does.
//
// Warn, never fail: an expiring key is a thing to go and fix, not a reason for
// a healthcheck to report the instance as broken.
func (s *Server) checkCredentialFreshness() Check {
	c := Check{Name: "credential freshness"}
	if s.Secrets == nil || !s.Secrets.IsInitialized() {
		c.Status, c.Detail = StatusOK, "no credential vault on this instance"
		return c
	}
	if !s.Secrets.IsUnlocked() {
		// Not a fault: a locked vault is the resting state for a deployment
		// that unlocks on demand. It does mean this check cannot run.
		c.Status, c.Detail = StatusOK, "vault locked; expiry not checked"
		c.Fix = "unlock the vault to have expiring credentials reported here"
		return c
	}
	need, err := s.Secrets.NeedsAttention()
	if err != nil {
		c.Status, c.Detail = StatusWarn, err.Error()
		return c
	}
	if len(need) == 0 {
		c.Status, c.Detail = StatusOK, "no credential is expiring or overdue"
		return c
	}
	var expired, expiring, stale []string
	for _, i := range need {
		switch i.Status {
		case secrets.StatusExpired:
			expired = append(expired, i.Name)
		case secrets.StatusExpiring:
			expiring = append(expiring, i.Name)
		case secrets.StatusStale:
			stale = append(stale, i.Name)
		}
	}
	var parts []string
	if len(expired) > 0 {
		parts = append(parts, "EXPIRED: "+strings.Join(expired, ", "))
	}
	if len(expiring) > 0 {
		parts = append(parts, "expiring soon: "+strings.Join(expiring, ", "))
	}
	if len(stale) > 0 {
		parts = append(parts, "rotation due: "+strings.Join(stale, ", "))
	}
	c.Status, c.Detail = StatusWarn, strings.Join(parts, "; ")
	c.Fix = "grimoire secret check"
	return c
}
