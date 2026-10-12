package api

import (
	"context"
	"os"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/metrics"
)

// A write must not wait minutes on a model. One memory write may call the model
// for fact extraction and again for the reconcile decision. Both calls share one
// deadline, GRIMOIRE_WRITE_MODEL_BUDGET (default 20s; 0 means no limit, as it
// was before). When the deadline passes, each call falls back to the rules it
// always had, and the response says so with model_skipped.

const defaultWriteModelBudget = 20 * time.Second

// writeModelBudget reads GRIMOIRE_WRITE_MODEL_BUDGET. A value that does not
// parse is the default, not no limit: a typo must not remove the bound.
func writeModelBudget() time.Duration {
	raw := os.Getenv("GRIMOIRE_WRITE_MODEL_BUDGET")
	if raw == "" {
		return defaultWriteModelBudget
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return defaultWriteModelBudget
	}
	return d
}

// writeBudget is the deadline one memory write shares across its model calls.
// A nil *writeBudget has no deadline and records nothing.
type writeBudget struct {
	ctx     context.Context
	cancel  context.CancelFunc
	skipped bool
}

// newWriteBudget starts a write's deadline, derived from the request context so
// a caller that disconnects also stops the model calls. Close it when the write
// ends.
func newWriteBudget(parent context.Context) *writeBudget {
	d := writeModelBudget()
	if d == 0 {
		return &writeBudget{ctx: context.Background(), cancel: func() {}}
	}
	ctx, cancel := context.WithTimeout(parent, d)
	return &writeBudget{ctx: ctx, cancel: cancel}
}

// context is the context the write's model calls run under.
func (b *writeBudget) context() context.Context {
	if b == nil {
		return context.Background()
	}
	return b.ctx
}

// note records a model call that the deadline cut short. It is called right
// after the call returns: a call that was still running when the deadline
// passed was skipped, and one that finished earlier was not.
func (b *writeBudget) note(stage string) {
	if b == nil || b.skipped || b.ctx.Err() != context.DeadlineExceeded {
		return
	}
	b.skipped = true
	metrics.Count("grimoire_memory_write_model_budget_total",
		"Memory writes whose model call was cut short by the write budget.",
		map[string]string{"stage": stage})
}

// wasSkipped reports whether the budget cut a model call short.
func (b *writeBudget) wasSkipped() bool { return b != nil && b.skipped }

// close releases the deadline.
func (b *writeBudget) close() {
	if b != nil && b.cancel != nil {
		b.cancel()
	}
}
