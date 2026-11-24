package api

import (
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// cleanInjected prepares memory text the server sends to an agent unasked:
// the hook's per-prompt context and the standing memory core. Commands are
// left in the note (memstore.CondenseCommands says why), and credentials are
// redacted twice: by shape, and against every value the vault holds.
// source, when set, is the note to point at for the commands that were left
// out.
func (s *Server) cleanInjected(text, source string) string {
	text, removed := memstore.CondenseCommands(text)
	if removed > 0 && source != "" {
		text = strings.TrimSpace(text) + " (commands left out; read_note " + source + " for them)"
	}
	text, _ = secrets.RedactText(text)
	if s.Secrets != nil {
		text, _ = s.Secrets.RedactKnown(text)
	}
	return text
}
