package api

import (
	"net/http"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// Scanning the vault for credentials that were pasted instead of stored.
//
// Admin-gated, because the result is a map of where this vault's weak points
// are. It carries no values — findings are masked at the source — but "there
// is an AWS key on line 40 of ops.md" is still precisely the sentence an
// attacker would most like to read.

func (s *Server) scanSecrets(w http.ResponseWriter, _ *http.Request) {
	paths, err := s.Vault.Walk()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	findings := []secrets.Finding{}
	scanned := 0
	for _, p := range paths {
		note, err := s.Vault.Read(p)
		if err != nil {
			continue // an unreadable note is not a finding
		}
		scanned++
		findings = append(findings, secrets.ScanText(p, note.Raw)...)
	}
	high := 0
	for _, f := range findings {
		if f.Confidence == secrets.ConfidenceHigh {
			high++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned":  scanned,
		"findings": findings,
		"high":     high,
		"scope": "Credentials found in NOTES, not in the vault. Values are masked: " +
			"a report that quoted them would copy the leak somewhere new.",
	})
}
