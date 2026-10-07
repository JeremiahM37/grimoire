package bank

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitOp(t *testing.T, e *Engine, bank, id string) *Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	op, err := e.WaitOperation(ctx, bank, id)
	if err != nil {
		t.Fatalf("waiting for %s: %v (op %+v)", id, err, op)
	}
	return op
}

func TestAsyncRetainRunsAndSurvivesARestart(t *testing.T) {
	h := newHarness(t, false)
	id, err := h.e.EnqueueRetain("b", []Item{{Content: "Alice adopted a beagle named Biscuit.", DocumentID: "d1"}}, RetainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The process "crashed" while running it.
	if err := h.ix.DB.Exec("UPDATE bank_operations SET status='running' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	// A new engine over the same index picks it up again on start.
	e2 := New(h.ix, h.v, h.e.AI, nil)
	h.ix.Banks = e2
	if err := e2.StartWorkers(2); err != nil {
		t.Fatal(err)
	}
	defer e2.StopWorkers()
	op := waitOp(t, e2, "b", id)
	if op.Status != OpCompleted || op.Attempts < 1 {
		t.Fatalf("op = %+v", op)
	}
	var res RetainResult
	if err := json.Unmarshal(op.Result, &res); err != nil || len(res.Documents) != 1 || res.Documents[0].Facts != 1 {
		t.Fatalf("result = %s %v", op.Result, err)
	}
	if f, _, _ := e2.ListFacts("b", FactQuery{}); len(f) != 1 {
		t.Errorf("facts = %v", f)
	}
}

func TestCancelStopsQueuedAndRunningOperations(t *testing.T) {
	h := newHarness(t, false)
	q := h.e.opsState()
	started := make(chan struct{}, 1)
	q.handlers["test_block"] = func(ctx context.Context, op *Operation) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return map[string]any{"stopped": true}, ctx.Err()
	}
	queued, _, _ := h.e.Enqueue("b", OpRetain, retainPayload{Items: []Item{{Content: "x"}}}, "")
	op, err := h.e.CancelOperation("b", queued)
	if err != nil || op.Status != OpCancelled {
		t.Fatalf("cancel queued = %+v %v", op, err)
	}
	if _, err := h.e.CancelOperation("b", queued); !errors.Is(err, ErrTerminal) {
		t.Errorf("cancelling twice = %v", err)
	}
	running, _, _ := h.e.Enqueue("b", "test_block", nil, "")
	h.e.StartWorkers(1)
	defer h.e.StopWorkers()
	<-started
	if _, err := h.e.CancelOperation("b", running); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	op, _ = h.e.GetOperation("b", running)
	if op.Status != OpCancelled || !op.CancelRequested {
		t.Errorf("running op after cancel = %+v", op)
	}
	if _, err := h.e.GetOperation("other", running); err != ErrNotFound {
		t.Errorf("an operation is only visible from its own bank: %v", err)
	}
}

func TestWorkersServeBanksFairly(t *testing.T) {
	h := newHarness(t, false)
	q := h.e.opsState()
	var mu sync.Mutex
	var order []string
	q.handlers["test_rec"] = func(_ context.Context, op *Operation) (any, error) {
		mu.Lock()
		order = append(order, op.BankID)
		mu.Unlock()
		return nil, nil
	}
	var last string
	for _, b := range []string{"a", "a", "a", "b"} {
		last, _, _ = h.e.Enqueue(b, "test_rec", nil, "")
	}
	first, _, _ := h.e.Enqueue("a", "test_rec", nil, "")
	h.e.StartWorkers(1)
	defer h.e.StopWorkers()
	waitOp(t, h.e, "b", last)
	waitOp(t, h.e, "a", first)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, "") != "abaaa" {
		t.Errorf("order = %v; the bank with one job must not wait behind four", order)
	}
}

func TestEnqueueDedupesAWaitingConsolidation(t *testing.T) {
	h := newHarness(t, true)
	a, d1, _ := h.e.EnqueueConsolidation("b")
	b, d2, _ := h.e.EnqueueConsolidation("b")
	if a != b || d1 || !d2 {
		t.Errorf("dedupe: %s %v / %s %v", a, d1, b, d2)
	}
}

func TestWebhookDeliveryIsSignedAndRetried(t *testing.T) {
	h := newHarness(t, false)
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	h.e.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	h.e.AllowPrivateWebhooks = func() bool { return true }
	var hits atomic.Int32
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotSig, gotBody = r.Header.Get("X-Grimoire-Signature"), string(body)
		mu.Unlock()
		if r.Header.Get("X-Grimoire-Event") != EventRetainCompleted {
			t.Errorf("event header = %q", r.Header.Get("X-Grimoire-Event"))
		}
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	h.retain(t, "b", Item{Content: "x y z.", DocumentID: "d"})
	wh, err := h.e.CreateWebhook(context.Background(), "b", WebhookSpec{URL: strPtr(srv.URL), Secret: strPtr("s3cret"),
		Events: &[]string{EventRetainCompleted}})
	if err != nil || wh.Secret != "s3cret" {
		t.Fatalf("create = %+v %v", wh, err)
	}
	list, _ := h.e.ListWebhooks("b")
	if len(list) != 1 || list[0].Secret != "" || !list[0].HasSecret {
		t.Errorf("a listing must never show the secret: %+v", list)
	}
	h.e.FireEvent("b", EventRetainCompleted, "op-1", OpCompleted, map[string]any{"document_ids": []string{"d"}})
	h.e.FireEvent("b", EventConsolidationCompleted, "op-2", OpCompleted, nil) // not subscribed
	ctx := context.Background()
	if n := h.e.DeliverDue(ctx); n != 1 {
		t.Fatalf("delivered %d", n)
	}
	d, _ := h.e.ListDeliveries("b", wh.ID, 10)
	if len(d) != 1 || d[0].Status != "pending" || d[0].Attempts != 1 || d[0].LastStatus != 502 {
		t.Fatalf("after one failure: %+v", d)
	}
	// Not due again until 5 s have passed.
	if n := h.e.DeliverDue(ctx); n != 0 {
		t.Errorf("retried before its delay: %d", n)
	}
	advance(5 * time.Second)
	h.e.DeliverDue(ctx)
	advance(4 * time.Minute)
	if n := h.e.DeliverDue(ctx); n != 0 {
		t.Error("the second retry waits five minutes")
	}
	advance(time.Minute)
	h.e.DeliverDue(ctx)
	d, _ = h.e.ListDeliveries("b", wh.ID, 10)
	if d[0].Status != "delivered" || d[0].Attempts != 3 || hits.Load() != 3 {
		t.Fatalf("after recovery: %+v hits=%d", d[0], hits.Load())
	}
	mu.Lock()
	sig, body := gotSig, gotBody
	mu.Unlock()
	if !VerifyWebhook("s3cret", sig, []byte(body), clock, time.Minute) {
		t.Errorf("signature %q does not verify", sig)
	}
	if VerifyWebhook("wrong", sig, []byte(body), clock, time.Minute) ||
		VerifyWebhook("s3cret", sig, []byte(body), clock.Add(time.Hour), time.Minute) {
		t.Error("a wrong secret or a stale timestamp must not verify")
	}
	var ev WebhookEvent
	if json.Unmarshal([]byte(body), &ev) != nil || ev.BankID != "b" || ev.OperationID != "op-1" {
		t.Errorf("body = %s", body)
	}
}

func TestWebhookGivesUpAfterTheLastRetry(t *testing.T) {
	h := newHarness(t, false)
	h.e.AllowPrivateWebhooks = func() bool { return true }
	h.e.WebhookDelays = []time.Duration{0, 0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	h.retain(t, "b", Item{Content: "x.", DocumentID: "d"})
	wh, _ := h.e.CreateWebhook(context.Background(), "b", WebhookSpec{URL: strPtr(srv.URL)})
	h.e.FireEvent("b", EventRetainCompleted, "", OpCompleted, nil)
	for i := 0; i < 5; i++ {
		h.e.DeliverDue(context.Background())
	}
	d, _ := h.e.ListDeliveries("b", wh.ID, 10)
	if d[0].Status != "failed" || d[0].Attempts != 3 {
		t.Errorf("delivery = %+v", d[0])
	}
}

func TestWebhookURLsAreGuarded(t *testing.T) {
	ctx := context.Background()
	for _, u := range []string{"http://127.0.0.1:9/", "http://localhost/x", "http://10.1.2.3/", "http://[::1]/",
		"http://2130706433/", "http://0x7f.1/", "http://100.64.0.1/"} {
		if err := ValidateWebhookURL(ctx, u, false); err == nil {
			t.Errorf("%s must be refused without the private opt-in", u)
		}
	}
	for _, u := range []string{"http://169.254.169.254/latest", "ftp://example.com/", "http://user:pw@example.com/", "/relative"} {
		if err := ValidateWebhookURL(ctx, u, true); err == nil {
			t.Errorf("%s must be refused even with the opt-in", u)
		}
	}
	if err := ValidateWebhookURL(ctx, "http://127.0.0.1:9/", true); err != nil {
		t.Errorf("loopback with the opt-in: %v", err)
	}
	// And at connect time: a webhook registered while the opt-in was on is
	// not delivered once it is off.
	h := newHarness(t, false)
	allow := true
	h.e.AllowPrivateWebhooks = func() bool { return allow }
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	h.retain(t, "b", Item{Content: "x.", DocumentID: "d"})
	wh, err := h.e.CreateWebhook(ctx, "b", WebhookSpec{URL: strPtr(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	allow = false
	h.e.FireEvent("b", EventRetainCompleted, "", OpCompleted, nil)
	h.e.DeliverDue(ctx)
	d, _ := h.e.ListDeliveries("b", wh.ID, 10)
	if hits.Load() != 0 || d[0].Status != "failed" || !strings.Contains(d[0].LastError, "refusing") {
		t.Errorf("delivery to a now-forbidden address: hits=%d %+v", hits.Load(), d[0])
	}
}

func TestOperationsFireWebhooksOnCompletion(t *testing.T) {
	h := newHarness(t, false)
	h.e.AllowPrivateWebhooks = func() bool { return true }
	got := make(chan WebhookEvent, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev WebhookEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		got <- ev
	}))
	defer srv.Close()
	if err := h.e.CreateBank(NewProfile("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.CreateWebhook(context.Background(), "b", WebhookSpec{URL: strPtr(srv.URL)}); err != nil {
		t.Fatal(err)
	}
	h.e.StartWorkers(1)
	defer h.e.StopWorkers()
	id, _ := h.e.EnqueueRetain("b", []Item{{Content: "Alice adopted a beagle.", DocumentID: "d1"}}, RetainOptions{})
	select {
	case ev := <-got:
		if ev.Event != EventRetainCompleted || ev.OperationID != id || ev.Status != OpCompleted || ev.Data["memory_unit_count"] != 1.0 {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no webhook")
	}
}
