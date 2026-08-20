package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/fts"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Agent memory — the substrate's agent-writable namespace.
// Port of server/routers/memory.py.
//
// Agents persist what they learn under memory/ as ordinary markdown notes with
// provenance frontmatter (memory: true, agent, task). Because memories ARE
// notes, everything the console offers applies: read, edit, diff, roll back,
// link, search. That human-auditable loop — your agent's memory is a note you
// can open — is the point of the design.

// MemoryDir is the namespace agent memories live in.
const MemoryDir = "memory"

// agentRE keeps an agent name to something safe to display and store; it lands
// in frontmatter and in a provenance banner.
var agentRE = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}_ .:/-]{0,60}$`)

type memoryIn struct {
	Text  string `json:"text"`
	Topic string `json:"topic"`
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

func (s *Server) memoryRel(topic string) string {
	slug := vault.Now().Format("2006-01-02")
	if strings.TrimSpace(topic) != "" {
		slug = vault.Slugify(topic)
	}
	return MemoryDir + "/" + slug + ".md"
}

// remember appends one memory, creating the topic note on first use.
func (s *Server) remember(w http.ResponseWriter, r *http.Request) {
	var m memoryIn
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" || len(text) > 20000 {
		writeErr(w, http.StatusBadRequest, "text must be 1..20000 characters")
		return
	}
	agent := strings.TrimSpace(m.Agent)
	if agent == "" {
		agent = "agent"
	}
	if !agentRE.MatchString(agent) {
		writeErr(w, http.StatusBadRequest, "invalid agent name")
		return
	}
	task := strings.TrimSpace(m.Task)
	rel := s.memoryRel(m.Topic)
	stamp := vault.Now().Format("2006-01-02 15:04")
	attribution := stamp + " · " + agent
	if task != "" {
		attribution += " · " + task
	}
	entry := "- **" + attribution + "** — " + text + "\n"

	existing, readErr := s.Vault.Read(rel)
	created := readErr != nil

	var fm *markdown.Frontmatter
	var body string
	if !created {
		// agent writes are rollbackable like any other edit
		s.History.Snapshot(rel, existing.Body)
		fm = existing.Frontmatter.Clone()
		fm.Set("agent", agent) // most recent writer…
		if task != "" {
			fm.Set("task", task) // …and their task, kept together
		}
		body = strings.TrimRight(existing.Body, "\n") + "\n" + entry
	} else {
		title := strings.TrimSpace(m.Topic)
		if title == "" {
			title = vault.Now().Format("2006-01-02")
		}
		fm = markdown.NewFrontmatter()
		fm.Set("title", "Memory: "+title)
		fm.Set("memory", true)
		fm.Set("agent", agent)
		if task != "" {
			fm.Set("task", task)
		}
		body = "# Memory: " + title + "\n\n" + entry
	}
	if _, err := s.Vault.Write(rel, body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"path": rel, "created": created, "entry": strings.TrimSpace(entry),
	})
}

type memoryOut struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Updated string `json:"updated"`
	Body    string `json:"body"`
}

// recall returns memories. With a query it runs FTS over the memory namespace;
// without one, the most recently touched memory notes. Full bodies are
// returned — memories exist to be re-read.
func (s *Server) recall(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	like := MemoryDir + "/%"

	var out []memoryOut
	if q != "" {
		// Memories are ordinary notes, so they live in spaces like any other
		// and one member must not recall another's.
		where, spaceArgs := s.whereSpace(r, "n.space",
			" WHERE n.path LIKE ? AND n.path IN (SELECT path FROM fts WHERE fts MATCH ?)")
		args := append(append([]any{like, fts.Terms(q)}, spaceArgs...), limit)
		rows, err := s.Index.DB.Query(
			"SELECT n.path, n.title, n.body, n.updated FROM notes n"+where+
				" ORDER BY n.updated DESC, n.path LIMIT ?", args...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, err = scanMemories(rows)
		rows.Close()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		if len(out) == 0 {
			// exact terms missed — fall back to semantic retrieval over the
			// memory namespace so paraphrased recalls still land
			hits, err := s.Index.RetrieveFor(q, limit*3, filterFor(r, true))
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			seen := map[string]bool{}
			for _, h := range hits {
				if !strings.HasPrefix(h.Path, MemoryDir+"/") || seen[h.Path] {
					continue
				}
				seen[h.Path] = true
				if len(out) >= limit {
					break
				}
				var m memoryOut
				if err := s.Index.DB.QueryRow(
					"SELECT path, title, body, updated FROM notes WHERE path=?", h.Path,
				).Scan(&m.Path, &m.Title, &m.Body, &m.Updated); err == nil {
					out = append(out, m)
				}
			}
		}
	} else {
		where, spaceArgs := s.whereSpace(r, "space", " WHERE path LIKE ?")
		args := append(append([]any{like}, spaceArgs...), limit)
		rows, err := s.Index.DB.Query(
			"SELECT path, title, body, updated FROM notes"+where+
				" ORDER BY updated DESC, path LIMIT ?", args...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, err = scanMemories(rows)
		rows.Close()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if out == nil {
		out = []memoryOut{}
	}
	writeJSON(w, http.StatusOK, out)
}

// scanMemories drains a memory query. It takes the concrete *sql.Rows rather
// than a Next/Scan interface so that Err() — the only way to tell "no more
// rows" apart from "iteration failed" — is reachable.
func scanMemories(rows *sql.Rows) ([]memoryOut, error) {
	var out []memoryOut
	for rows.Next() {
		var m memoryOut
		if err := rows.Scan(&m.Path, &m.Title, &m.Body, &m.Updated); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// briefing is the "read this first" pack for an agent joining a session:
// pinned notes, onboarding-tagged notes, and the most recent memories, in one
// call — standing context arrives unprompted instead of being hunted for.
func (s *Server) briefing(w http.ResponseWriter, r *http.Request) {
	n := 5
	if v := r.URL.Query().Get("memories"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 {
			n = k
		}
	}
	collect := func(q string, args ...any) ([]map[string]string, error) {
		out := []map[string]string{}
		rows, err := s.Index.DB.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var path, title, body string
			if err := rows.Scan(&path, &title, &body); err != nil {
				return nil, err
			}
			// The briefing is standing context handed to an agent before it
			// asks for anything, so it is the surface most likely to leak a
			// space quietly. Every bucket goes through this one filter.
			if !s.canRead(r, path) {
				continue
			}
			out = append(out, map[string]string{"path": path, "title": title, "body": body})
		}
		return out, rows.Err()
	}
	// Python stores frontmatter JSON with a space after the colon, so the LIKE
	// pattern must match that shape exactly or no note ever looks pinned.
	// A briefing that silently drops a bucket is worse than one that fails:
	// the agent cannot tell "nothing is pinned" from "the pinned query broke".
	pinned, err := collect(
		"SELECT path, title, body FROM notes WHERE private=0 " +
			"AND frontmatter_json LIKE '%\"pinned\": true%' ORDER BY updated DESC, path LIMIT 10")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	onboarding, err := collect(
		"SELECT n.path, n.title, n.body FROM notes n JOIN tags t ON t.note=n.path " +
			"WHERE t.tag='onboarding' AND n.private=0 ORDER BY n.updated DESC, n.path LIMIT 10")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	recent, err := collect(
		"SELECT path, title, body FROM notes WHERE path LIKE ? AND private=0 "+
			"ORDER BY updated DESC, path LIMIT ?", MemoryDir+"/%", n)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// A note appearing in more than one bucket is listed once, in the first
	// bucket that claims it — the briefing is a reading list, not a join.
	seen := map[string]bool{}
	dedupe := func(rows []map[string]string) []map[string]string {
		out := []map[string]string{}
		for _, r := range rows {
			if seen[r["path"]] {
				continue
			}
			seen[r["path"]] = true
			out = append(out, r)
		}
		return out
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pinned":          dedupe(pinned),
		"onboarding":      dedupe(onboarding),
		"recent_memories": dedupe(recent),
	})
}
