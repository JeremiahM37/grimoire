package bank

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// Asynchronous operations: work a caller hands off and polls for — a retain
// too large to wait on, a consolidation, a mental-model refresh.
//
// Operations are rows in bank_operations, which is operational state like the
// audit log: not derived from the vault, and kept across a reindex. A small
// in-process pool of workers runs them. Two rules shape the scheduling:
//
//   - one operation per bank at a time. Every kind writes the bank's files
//     under its lock anyway, so running two would only queue them on the
//     lock while holding a worker; serialising here frees the worker for
//     another bank instead.
//   - among banks with work waiting, the one served least recently goes
//     next, so a bank with a thousand queued retains cannot starve one with
//     a single consolidation.
//
// Cancellation is cooperative: a queued operation never starts; a running one
// is marked cancelled at once and its context is cancelled, which the work
// notices at its next check. A server restart finds operations it was running
// marked running, and puts them back in the queue.

// Operation statuses.
const (
	OpQueued    = "queued"
	OpRunning   = "running"
	OpCompleted = "completed"
	OpFailed    = "failed"
	OpCancelled = "cancelled"
)

// Operation kinds.
const (
	OpRetain        = "retain"
	OpConsolidation = "consolidation"
	OpRefreshModel  = "refresh_mental_model"
)

// ErrTerminal is returned when cancelling an operation that already ended.
var ErrTerminal = errors.New("operation already finished")

// Operation is one queued, running or finished piece of work.
type Operation struct {
	ID     string `json:"id"`
	BankID string `json:"bank_id"`
	Kind   string `json:"kind"`
	// Type repeats Kind under the name list filters use (?type=).
	Type            string          `json:"type"`
	Status          string          `json:"status"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
	Attempts        int             `json:"attempts"`
	CancelRequested bool            `json:"cancel_requested,omitempty"`
	Progress        string          `json:"progress,omitempty"`
	Created         string          `json:"created_at"`
	Started         string          `json:"started_at,omitempty"`
	Finished        string          `json:"finished_at,omitempty"`
	Updated         string          `json:"updated_at,omitempty"`
}

// opHandler runs one operation and returns its result.
type opHandler func(ctx context.Context, op *Operation) (any, error)

// ops is the engine's queue state.
type ops struct {
	mu       sync.Mutex
	wake     chan struct{}
	cancels  map[string]context.CancelFunc
	busy     map[string]bool  // banks with an operation running
	served   map[string]int64 // when each bank last had one started
	seq      int64
	handlers map[string]opHandler
	started  bool
	stop     chan struct{}
	wg       sync.WaitGroup
}

func (e *Engine) opsState() *ops {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.q == nil {
		e.q = &ops{wake: make(chan struct{}, 1), cancels: map[string]context.CancelFunc{},
			busy: map[string]bool{}, served: map[string]int64{}}
		e.q.handlers = map[string]opHandler{
			OpRetain:        e.runRetainOp,
			OpConsolidation: e.runConsolidationOp,
			OpRefreshModel:  e.runRefreshOp,
		}
	}
	return e.q
}

func newOpID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return "op-" + hex.EncodeToString(b)
}

func msTime(v int64) string {
	if v == 0 {
		return ""
	}
	return time.UnixMilli(v).UTC().Format(time.RFC3339Nano)
}

// Enqueue adds an operation. With a dedupe key, an operation of the same key
// still waiting in the queue is returned instead (deduped true).
func (e *Engine) Enqueue(bankID, kind string, payload any, dedupe string) (id string, deduped bool, err error) {
	if !ValidID(bankID) {
		return "", false, invalid("invalid bank id")
	}
	q := e.opsState()
	if _, ok := q.handlers[kind]; !ok {
		return "", false, invalid("unknown operation kind %q", kind)
	}
	raw := []byte("{}")
	if payload != nil {
		if raw, err = json.Marshal(payload); err != nil {
			return "", false, err
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if dedupe != "" {
		var existing string
		err := e.Index.DB.QueryRow("SELECT id FROM bank_operations WHERE bank=? AND dedupe=? AND status=? ORDER BY created LIMIT 1",
			bankID, dedupe, OpQueued).Scan(&existing)
		if err == nil {
			return existing, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
	}
	id = newOpID()
	now := e.now().UnixMilli()
	// created is made strictly increasing so the queue order is the order of
	// submission even within one millisecond.
	q.seq = max(q.seq+1, now)
	if err := e.Index.DB.Exec("INSERT INTO bank_operations(id,bank,kind,status,payload,dedupe,created,updated) VALUES(?,?,?,?,?,?,?,?)",
		id, bankID, kind, OpQueued, string(raw), dedupe, q.seq, now); err != nil {
		return "", false, err
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return id, false, nil
}

const opColumns = "id,bank,kind,status,payload,result,error,attempts,cancel_requested,progress,created,started,finished,updated"

func scanOp(sc interface{ Scan(...any) error }) (*Operation, error) {
	var o Operation
	var payload, result string
	var cancel int
	var created, started, finished, updated int64
	if err := sc.Scan(&o.ID, &o.BankID, &o.Kind, &o.Status, &payload, &result, &o.Error, &o.Attempts, &cancel,
		&o.Progress, &created, &started, &finished, &updated); err != nil {
		return nil, err
	}
	if payload != "" {
		o.Payload = json.RawMessage(payload)
	}
	if result != "" {
		o.Result = json.RawMessage(result)
	}
	o.CancelRequested = cancel == 1
	o.Type = o.Kind
	o.Created, o.Started, o.Finished, o.Updated = msTime(created), msTime(started), msTime(finished), msTime(updated)
	return &o, nil
}

// GetOperation returns one operation of a bank.
func (e *Engine) GetOperation(bankID, id string) (*Operation, error) {
	row := e.Index.DB.QueryRow("SELECT "+opColumns+" FROM bank_operations WHERE id=? AND bank=?", id, bankID)
	o, err := scanOp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

// OperationQuery filters an operation listing.
type OperationQuery struct {
	Status, Kind  string
	Limit, Offset int
}

// ListOperations lists a bank's operations, newest first. Payloads are left
// out of listings; GetOperation has them.
func (e *Engine) ListOperations(bankID string, q OperationQuery) ([]Operation, int, error) {
	where := "bank=?"
	args := []any{bankID}
	if q.Status != "" {
		where += " AND status=?"
		args = append(args, q.Status)
	}
	if q.Kind != "" {
		where += " AND kind=?"
		args = append(args, q.Kind)
	}
	total, err := e.Index.DB.Count("SELECT COUNT(*) FROM bank_operations WHERE "+where, args...)
	if err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := e.Index.DB.Query("SELECT "+opColumns+" FROM bank_operations WHERE "+where+
		" ORDER BY created DESC LIMIT ? OFFSET ?", append(args, limit, max(q.Offset, 0))...)
	if err != nil {
		return nil, 0, err
	}
	out := []Operation{}
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		o.Payload = nil
		out = append(out, *o)
	}
	rows.Close()
	return out, total, nil
}

// OperationCounts reports a bank's operations by status.
func (e *Engine) OperationCounts(bankID string) map[string]int {
	out := map[string]int{}
	rows, err := e.Index.DB.Query("SELECT status, COUNT(*) FROM bank_operations WHERE bank=? GROUP BY status", bankID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

// CancelOperation stops an operation: a queued one never starts, a running
// one is marked cancelled now and told to stop.
func (e *Engine) CancelOperation(bankID, id string) (*Operation, error) {
	q := e.opsState()
	q.mu.Lock()
	op, err := e.GetOperation(bankID, id)
	if err != nil {
		q.mu.Unlock()
		return nil, err
	}
	switch op.Status {
	case OpCompleted, OpFailed, OpCancelled:
		q.mu.Unlock()
		return op, ErrTerminal
	}
	now := e.now().UnixMilli()
	err = e.Index.DB.Exec("UPDATE bank_operations SET status=?, cancel_requested=1, finished=?, updated=? WHERE id=?",
		OpCancelled, now, now, id)
	cancel := q.cancels[id]
	q.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if cancel != nil {
		cancel()
	}
	return e.GetOperation(bankID, id)
}

// StartWorkers recovers interrupted operations and starts n workers plus the
// webhook delivery loop. Calling it again is a no-op.
func (e *Engine) StartWorkers(n int) error {
	q := e.opsState()
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return nil
	}
	q.started = true
	q.stop = make(chan struct{})
	q.mu.Unlock()
	// Whatever was running when the process stopped did not finish: run it
	// again. Every kind is safe to repeat — retain is idempotent on its
	// document, consolidation picks up the facts not yet read, and a refresh
	// rewrites an answer.
	if n, err := e.Index.DB.ExecAffected("UPDATE bank_operations SET status=?, started=0 WHERE status=?", OpQueued, OpRunning); err != nil {
		return err
	} else if n > 0 {
		log.Printf("banks: re-queued %d operations interrupted by a restart", n)
	}
	if n <= 0 {
		n = 2
	}
	for i := 0; i < n; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			e.workLoop(q)
		}()
	}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		e.deliveryLoop(q.stop)
	}()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

// StopWorkers stops the workers and waits for running operations to return.
// Their contexts are cancelled, so a long one stops at its next check and is
// re-queued by the next StartWorkers.
func (e *Engine) StopWorkers() {
	q := e.opsState()
	q.mu.Lock()
	if !q.started {
		q.mu.Unlock()
		return
	}
	q.started = false
	close(q.stop)
	for _, c := range q.cancels {
		c()
	}
	q.mu.Unlock()
	q.wg.Wait()
}

func (e *Engine) workLoop(q *ops) {
	for {
		select {
		case <-q.stop:
			return
		default:
		}
		op, ctx, err := e.claimNext(q)
		if err != nil {
			log.Printf("banks: claiming an operation: %v", err)
		}
		if op == nil {
			select {
			case <-q.stop:
				return
			case <-q.wake:
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		e.runOp(q, ctx, op)
		select {
		case q.wake <- struct{}{}: // another worker may now take this bank's next
		default:
		}
	}
}

// claimNext takes the next operation, fairly across banks.
func (e *Engine) claimNext(q *ops) (*Operation, context.Context, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	select {
	case <-q.stop:
		return nil, nil, nil
	default:
	}
	rows, err := e.Index.DB.Query("SELECT id, bank FROM bank_operations WHERE status=? ORDER BY created LIMIT 500", OpQueued)
	if err != nil {
		return nil, nil, err
	}
	type head struct{ id, bank string }
	var heads []head
	seen := map[string]bool{}
	for rows.Next() {
		var h head
		if rows.Scan(&h.id, &h.bank) != nil {
			continue
		}
		if seen[h.bank] || q.busy[h.bank] {
			continue
		}
		seen[h.bank] = true
		heads = append(heads, h)
	}
	rows.Close()
	if len(heads) == 0 {
		return nil, nil, nil
	}
	sort.SliceStable(heads, func(a, b int) bool { return q.served[heads[a].bank] < q.served[heads[b].bank] })
	h := heads[0]
	now := e.now().UnixMilli()
	n, err := e.Index.DB.ExecAffected("UPDATE bank_operations SET status=?, started=?, updated=?, attempts=attempts+1 WHERE id=? AND status=?",
		OpRunning, now, now, h.id, OpQueued)
	if err != nil || n == 0 {
		return nil, nil, err
	}
	op, err := e.GetOperation(h.bank, h.id)
	if err != nil {
		return nil, nil, err
	}
	q.busy[h.bank] = true
	q.seq++
	q.served[h.bank] = q.seq
	ctx, cancel := context.WithCancel(context.Background())
	q.cancels[op.ID] = cancel
	return op, ctx, nil
}

func (e *Engine) runOp(q *ops, ctx context.Context, op *Operation) {
	handler := q.handlers[op.Kind]
	var result any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("operation panicked: %v", r)
			}
		}()
		result, err = handler(ctx, op)
	}()
	q.mu.Lock()
	cancel := q.cancels[op.ID]
	delete(q.cancels, op.ID)
	delete(q.busy, op.BankID)
	q.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var raw []byte
	if result != nil {
		raw, _ = json.Marshal(result)
	}
	status, msg := OpCompleted, ""
	if err != nil {
		status, msg = OpFailed, err.Error()
		if errors.Is(err, ErrModelRequired) {
			msg = "model_required: this operation needs a language model and none is configured"
		}
	}
	now := e.now().UnixMilli()
	// A cancelled operation stays cancelled whatever its work returned.
	n, uerr := e.Index.DB.ExecAffected("UPDATE bank_operations SET status=?, result=?, error=?, finished=?, updated=? WHERE id=? AND status=?",
		status, string(raw), runeCut(msg, 5000), now, now, op.ID, OpRunning)
	if uerr != nil {
		log.Printf("banks: recording operation %s: %v", op.ID, uerr)
		return
	}
	if n == 0 {
		// Cancelled while running (or stopped for shutdown, which re-queues):
		// keep what the work produced for the record, but not the status.
		if raw != nil {
			_ = e.Index.DB.Exec("UPDATE bank_operations SET result=? WHERE id=?", string(raw), op.ID)
		}
		select {
		case <-q.stop:
			// Shutting down: put it back so the next start runs it.
			_ = e.Index.DB.Exec("UPDATE bank_operations SET status=?, started=0 WHERE id=? AND status=?", OpQueued, op.ID, OpRunning)
		default:
		}
		return
	}
	e.opFinished(op, status, result, err)
}

// opFinished fires the webhook an operation's end announces.
func (e *Engine) opFinished(op *Operation, status string, result any, err error) {
	data := map[string]any{}
	if err != nil {
		data["error_message"] = err.Error()
	}
	switch op.Kind {
	case OpRetain:
		if r, ok := result.(*RetainResult); ok && r != nil {
			var docs []string
			facts := 0
			for _, d := range r.Documents {
				docs = append(docs, d.DocumentID)
				facts += d.Facts
			}
			data["document_ids"], data["memory_unit_count"], data["items_count"] = docs, facts, r.ItemsCount
		}
		e.FireEvent(op.BankID, EventRetainCompleted, op.ID, status, data)
	case OpConsolidation:
		if r, ok := result.(*ConsolidationResult); ok && r != nil {
			data["observations_created"], data["observations_updated"] = r.Created, r.Updated
			data["observations_deleted"], data["challenges_recorded"] = r.Deleted, r.Challenges
			data["facts_processed"] = r.FactsProcessed
		}
		e.FireEvent(op.BankID, EventConsolidationCompleted, op.ID, status, data)
	}
}

// ------------------------------------------------------------ handlers

type retainPayload struct {
	Items []Item        `json:"items"`
	Opts  RetainOptions `json:"options"`
}

func (e *Engine) runRetainOp(ctx context.Context, op *Operation) (any, error) {
	var p retainPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return nil, fmt.Errorf("bad retain payload: %w", err)
	}
	res, err := e.Retain(ctx, op.BankID, p.Items, p.Opts)
	if err != nil {
		return res, err
	}
	return res, nil
}

// EnqueueRetain queues a retain to run in the background.
func (e *Engine) EnqueueRetain(bankID string, items []Item, opts RetainOptions) (string, error) {
	if len(items) == 0 {
		return "", invalid("items must not be empty")
	}
	for i, it := range items {
		if it.Content == "" {
			return "", invalid("item %d has no content", i)
		}
	}
	// Strip before queueing: the payload is stored, so private text must never reach it.
	if items, _ = sanitizeItemsPII(items, e.piiMode(bankID)); len(items) == 0 {
		return "", invalid("nothing to retain after removing private text")
	}
	// Banks are created on first use (see Retain). An asynchronous retain has
	// to do that now, not in the worker: otherwise the operation it returns is
	// for a bank that does not exist yet, and every read route, which confirms
	// the bank before answering, reports the operation as not found until the
	// worker gets to it.
	if err := e.ensureBank(bankID); err != nil {
		return "", err
	}
	id, _, err := e.Enqueue(bankID, OpRetain, retainPayload{Items: items, Opts: opts}, "")
	return id, err
}

// ensureBank creates an empty bank when none exists, the same first-use rule
// Retain applies.
func (e *Engine) ensureBank(bankID string) error {
	if !ValidID(bankID) {
		return invalid("invalid bank id")
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := e.Profile(bankID); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	return e.writeProfile(NewProfile(bankID), false)
}

func (e *Engine) runConsolidationOp(ctx context.Context, op *Operation) (any, error) {
	res, err := e.Consolidate(ctx, op.BankID)
	if err != nil {
		return res, err
	}
	return res, nil
}

type refreshPayload struct {
	ModelID string `json:"mental_model_id"`
	Mode    string `json:"mode,omitempty"`
}

func (e *Engine) runRefreshOp(ctx context.Context, op *Operation) (any, error) {
	var p refreshPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return nil, fmt.Errorf("bad refresh payload: %w", err)
	}
	return e.RefreshModelWith(ctx, op.BankID, p.ModelID, RefreshOpts{Mode: p.Mode})
}

// EnqueueRefresh queues a mental-model refresh; one waiting refresh of a
// model absorbs another.
func (e *Engine) EnqueueRefresh(bankID, modelID string) (string, bool, error) {
	return e.EnqueueRefreshMode(bankID, modelID, "")
}

// EnqueueRefreshMode queues a refresh with an explicit mode (full or delta).
func (e *Engine) EnqueueRefreshMode(bankID, modelID, mode string) (string, bool, error) {
	return e.Enqueue(bankID, OpRefreshModel, refreshPayload{ModelID: modelID, Mode: mode}, "refresh:"+modelID)
}

// EnqueueConsolidation queues a consolidation; one already waiting absorbs
// the request.
func (e *Engine) EnqueueConsolidation(bankID string) (string, bool, error) {
	if !e.AI.Available() {
		return "", false, ErrModelRequired
	}
	return e.Enqueue(bankID, OpConsolidation, nil, "consolidation")
}

// WaitOperation polls until an operation ends or ctx is done. For tests and
// for callers that want a synchronous answer from the queue.
func (e *Engine) WaitOperation(ctx context.Context, bankID, id string) (*Operation, error) {
	for {
		op, err := e.GetOperation(bankID, id)
		if err != nil {
			return nil, err
		}
		switch op.Status {
		case OpCompleted, OpFailed, OpCancelled:
			return op, nil
		}
		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
