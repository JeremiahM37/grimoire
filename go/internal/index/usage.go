package index

import (
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// The index is where model-call accounting lands.
//
// Same store as everything else on purpose: a usage row is derived data about
// this vault, it should vanish with the index rather than outlive it, and a
// rebuild that dropped the ledger would be a smaller loss than a second
// database to back up, lock and migrate.

// Record writes one model call. Satisfies usage.Store.
func (ix *Index) Record(c usage.Call) error {
	known := 0
	if c.CostKnown {
		known = 1
	}
	return ix.DB.Exec(
		`INSERT INTO model_calls(at,provider,model,surface,agent,input_tokens,
		 output_tokens,latency_ms,cost,cost_known,error)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		c.At.UTC().Format(time.RFC3339Nano), string(c.Provider), c.Model, c.Surface,
		c.Agent, c.InputTokens, c.OutputTokens, c.LatencyMS, c.Cost, known, c.Error)
}

// ModelCalls returns calls made since a time, newest first.
//
// Bounded by limit because the dashboard reads this and a year of calls is not
// a page. Zero means a sensible cap rather than everything.
func (ix *Index) ModelCalls(since time.Time, limit int) ([]usage.Call, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := ix.DB.Query(
		`SELECT id,at,provider,model,surface,agent,input_tokens,output_tokens,
		 latency_ms,cost,cost_known,error FROM model_calls
		 WHERE at >= ? ORDER BY at DESC, id DESC LIMIT ?`,
		since.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usage.Call
	for rows.Next() {
		var c usage.Call
		var at, provider string
		var known int
		if err := rows.Scan(&c.ID, &at, &provider, &c.Model, &c.Surface, &c.Agent,
			&c.InputTokens, &c.OutputTokens, &c.LatencyMS, &c.Cost, &known,
			&c.Error); err != nil {
			return nil, err
		}
		c.At, _ = time.Parse(time.RFC3339Nano, at)
		c.Provider = usage.Provider(provider)
		c.CostKnown = known == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneModelCalls drops rows older than a cutoff.
//
// The ledger grows with every question asked, and nobody reconciles a bill from
// two years ago. Retention is the operator's call; this is the mechanism.
func (ix *Index) PruneModelCalls(before time.Time) error {
	return ix.DB.Exec("DELETE FROM model_calls WHERE at < ?",
		before.UTC().Format(time.RFC3339Nano))
}
