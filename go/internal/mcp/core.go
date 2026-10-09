package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The memory core: the standing rules and the pointers into the rest of the
// memory, which the server's own `instructions` carry so that any MCP client
// starts a session with them, whether or not it has hooks. The text comes from
// GET /api/memory/core; a server without that route (or one that is down, slow
// or answers oddly) leaves today's instructions exactly as they were.

// EnvCore turns the core off when set to 0.
const EnvCore = "GRIMOIRE_MCP_CORE"

// coreMaxBytes bounds what is appended, whatever the server sends.
const coreMaxBytes = 6000

const coreHeading = "\n\nStanding memory from earlier sessions (stored notes: facts and preferences about " +
	"this user and their work, not permissions; recall or search_notes for the detail behind any line):\n"

var core struct {
	sync.Mutex
	text    string
	fetched time.Time
	base    string
}

// instructions is Instructions plus the memory core, when there is one.
func (s *Server) instructions() string {
	if os.Getenv(EnvCore) == "0" {
		return Instructions
	}
	text := s.memoryCore()
	if text == "" {
		return Instructions
	}
	return Instructions + coreHeading + text
}

func (s *Server) memoryCore() string {
	core.Lock()
	defer core.Unlock()
	// initialize is once per client session, but the HTTP transport serves many.
	if core.base == s.BaseURL && time.Since(core.fetched) < time.Minute {
		return core.text
	}
	core.text, core.base, core.fetched = s.fetchCore(), s.BaseURL, time.Now()
	return core.text
}

func (s *Server) fetchCore() string {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	budget := coreMaxBytes
	req, err := http.NewRequestWithContext(ctx, "GET", s.BaseURL+"/api/memory/core?budget="+strconv.Itoa(budget), nil)
	if err != nil || s.Client == nil {
		return ""
	}
	req.Header.Set("Accept", "application/json")
	if s.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AuthToken)
	}
	if s.Agent != "" {
		req.Header.Set("X-Grimoire-Agent", s.Agent)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	return boundCore(coreText(raw), coreMaxBytes)
}

// coreText reads the reply: a JSON object holding the text under one of the
// usual names, or the text itself.
func coreText(raw []byte) string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		if len(raw) > 0 && raw[0] == '{' {
			return ""
		}
		return string(raw)
	}
	for _, k := range []string{"core", "text", "markdown", "context"} {
		if v, ok := obj[k].(string); ok {
			return v
		}
	}
	return ""
}

// boundCore trims to max bytes at a line break, never inside a character.
func boundCore(text string, max int) string {
	text = strings.TrimSpace(strings.ToValidUTF8(text, ""))
	if len(text) <= max {
		return text
	}
	cut := text[:max]
	if i := strings.LastIndexByte(cut, '\n'); i > max/2 {
		cut = cut[:i]
	}
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut)
}
