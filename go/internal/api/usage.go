package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// What grimoire's own model calls cost, and what agents have been doing.
//
// The boundary is stated in the response rather than left for a reader to
// infer, because the obvious misreading is expensive: this is NOT an agent's
// total AI spend. Grimoire is mounted BY agents and never sees the conversation
// they have with their own provider. It reports the calls it made itself —
// answering, reranking, classifying — on a key the operator configured.

// usageWindow parses ?since= into a start time. Defaults to 30 days, which is
// the period a bill covers.
func usageWindow(r *http.Request) (time.Time, string) {
	spec := strings.TrimSpace(r.URL.Query().Get("since"))
	now := time.Now().UTC()
	switch spec {
	case "24h", "day":
		return now.Add(-24 * time.Hour), "24h"
	case "7d", "week":
		return now.AddDate(0, 0, -7), "7d"
	case "all":
		return time.Time{}, "all"
	case "90d":
		return now.AddDate(0, 0, -90), "90d"
	default:
		return now.AddDate(0, 0, -30), "30d"
	}
}

// modelUsage reports the rollup the dashboard renders.
func (s *Server) modelUsage(w http.ResponseWriter, r *http.Request) {
	since, window := usageWindow(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	calls, err := s.Index.ModelCalls(since, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := usage.Summarise(calls, since)

	recent := calls
	if len(recent) > 50 {
		recent = recent[:50]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window":  window,
		"summary": sum,
		"recent":  recent,
		// Said in the payload, not only in the docs. A client that renders the
		// number without this caption is reporting something untrue.
		"scope": "Model calls Grimoire made itself — answering, reranking, " +
			"classifying. Not an agent's own token spend with its provider, " +
			"which does not pass through this server.",
		"prices_updated": usage.PricesUpdated,
	})
}

// agentActivity reports what each agent has done to the knowledge base.
//
// The other half of the question, and the half whose data grimoire genuinely
// owns end to end: who wrote which facts, who spent which credentials, what is
// still contested.
func (s *Server) agentActivity(w http.ResponseWriter, r *http.Request) {
	since, window := usageWindow(r)

	type agentRow struct {
		Agent      string    `json:"agent"`
		Facts      int       `json:"facts"`
		Challenges int       `json:"challenges"`
		LastSeen   string    `json:"last_seen,omitempty"`
		Calls      int       `json:"model_calls"`
		Cost       float64   `json:"model_cost"`
		FirstSeen  string    `json:"first_seen,omitempty"`
		_          time.Time `json:"-"`
	}
	rows := map[string]*agentRow{}
	get := func(name string) *agentRow {
		if name == "" {
			name = "(unattributed)"
		}
		if g, ok := rows[name]; ok {
			return g
		}
		g := &agentRow{Agent: name}
		rows[name] = g
		return g
	}

	// Facts written, and which are contested — straight from the memory table.
	memRows, err := s.Index.DB.Query(
		`SELECT agent, COUNT(*), SUM(CASE WHEN challenges!='' THEN 1 ELSE 0 END),
		        MIN(stamp), MAX(stamp)
		 FROM memory_entries WHERE superseded_by='' GROUP BY agent`)
	if err == nil {
		defer memRows.Close()
		for memRows.Next() {
			var agent, first, last string
			var n, chal int
			if err := memRows.Scan(&agent, &n, &chal, &first, &last); err != nil {
				break
			}
			g := get(agent)
			g.Facts, g.Challenges, g.FirstSeen, g.LastSeen = n, chal, first, last
		}
	}

	// Model calls attributed to an agent.
	calls, _ := s.Index.ModelCalls(since, 5000)
	for _, c := range calls {
		g := get(c.Agent)
		g.Calls++
		g.Cost += c.Cost
	}

	out := make([]agentRow, 0, len(rows))
	for _, g := range rows {
		out = append(out, *g)
	}
	// Busiest first: the agent worth looking at is the one doing the most.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Facts+out[j].Calls > out[j-1].Facts+out[j-1].Calls; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}

	// Credential use is the spend grimoire genuinely brokered, as opposed to
	// tokens it merely bought.
	var credUses int
	_ = s.Index.DB.QueryRow("SELECT COUNT(*) FROM audit").Scan(&credUses)

	writeJSON(w, http.StatusOK, map[string]any{
		"window":          window,
		"agents":          out,
		"credential_uses": credUses,
		"scope": "What agents did to this knowledge base. Credential uses are " +
			"calls Grimoire made on an agent's behalf with a secret it never saw.",
	})
}

// agentFor names the caller for attribution.
//
// A verified network identity wins over anything the caller said about itself.
// That is the point of resolving one: this name reaches the usage ledger, the
// read-audit trail and the authority lattice that decides whether a human's
// correction outranks an agent's rewrite, and until an overlay could vouch for
// it the name was simply whatever the caller typed in a header.
//
// Absent a verified identity the header is used exactly as before. MCP clients
// set GRIMOIRE_AGENT_NAME and the server forwards it, so a bill decomposes by
// which agent caused the work. A request with neither is the console or a
// background job and is attributed to nobody rather than being guessed at — a
// wrong name in a ledger is worse than an absent one.
func agentFor(r *http.Request) string {
	if r == nil {
		return ""
	}
	if name, ok := verifiedAgent(r); ok {
		return name
	}
	return claimedAgent(r)
}

// claimedAgent is what the caller says it is, with nothing checking.
//
// Kept separate from agentFor so the two can be compared: /whoami reports both,
// because a claim that disagrees with a verified identity is the signal an
// operator needs and a single merged value would hide it.
func claimedAgent(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, h := range []string{"X-Grimoire-Agent", "X-Agent-Name"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("agent"))
}
