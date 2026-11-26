package api

import (
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// commandsNote follows injected text whose commands were left in the note.
// The line already names the note, so it does not repeat the path.
const commandsNote = " (commands in the note)"

// cleanInjected prepares memory text the server sends to an agent unasked:
// the hook's per-prompt context and the standing memory core. Commands are
// left in the note (memstore.CondenseCommands says why), and credentials are
// redacted twice: by shape, and against every value the vault holds.
// source, when set, means the line names its note, so the text says when
// commands were left out.
func (s *Server) cleanInjected(text, source string) string {
	text, removed := s.cleanInjectedParts(text)
	if removed > 0 && source != "" {
		text = strings.TrimSpace(text) + commandsNote
	}
	return text
}

// cleanInjectedParts is cleanInjected without the closing note, and says how
// many commands it left out, for a caller that cuts the text further first.
func (s *Server) cleanInjectedParts(text string) (string, int) {
	text, removed := memstore.CondenseCommands(text)
	text, _ = secrets.RedactText(text)
	if s.Secrets != nil {
		text, _ = s.Secrets.RedactKnown(text)
	}
	return text, removed
}
