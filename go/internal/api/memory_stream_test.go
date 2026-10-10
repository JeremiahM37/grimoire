package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// The realtime memory stream. These run a real HTTP server, because the thing
// under test is the connection: frames arriving as they happen, a resume after
// a disconnect, and a goroutine that ends when its client leaves.

// fastStream shortens the poll and heartbeat intervals for one test.
func fastStream(t *testing.T, poll, beat time.Duration) {
	t.Helper()
	oldPoll, oldBeat := streamPoll, streamHeartbeat
	streamPoll, streamHeartbeat = poll, beat
	t.Cleanup(func() { streamPoll, streamHeartbeat = oldPoll, oldBeat })
}

type sseFrame struct {
	Comment, ID, Event, Data string
}

type sseConn struct {
	frames chan sseFrame
	cancel context.CancelFunc
	body   io.Closer
}

func (c *sseConn) close() {
	c.cancel()
	c.body.Close()
}

// openSSE connects and returns once the response headers have arrived, which
// the server only sends after it has settled its position. Anything written
// after this returns is an event the stream must deliver.
func openSSE(t *testing.T, base, path, key, lastID string, client *http.Client) *sseConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		resp.Body.Close()
		t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		resp.Body.Close()
		t.Fatalf("content type %q", ct)
	}
	c := &sseConn{frames: make(chan sseFrame, 256), cancel: cancel, body: resp.Body}
	go func() {
		defer close(c.frames)
		sc := bufio.NewScanner(resp.Body)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f != (sseFrame{}) {
					c.frames <- f
				}
				f = sseFrame{}
			case strings.HasPrefix(line, ":"):
				c.frames <- sseFrame{Comment: strings.TrimSpace(strings.TrimPrefix(line, ":"))}
			case strings.HasPrefix(line, "id: "):
				f.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	t.Cleanup(c.close)
	return c
}

// nextEvent waits for the next real event (not a comment), failing the test if
// none arrives in time.
func nextEvent(t *testing.T, c *sseConn) sseFrame {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				t.Fatal("stream ended while waiting for an event")
			}
			if f.Event != "" {
				return f
			}
		case <-deadline:
			t.Fatal("no event within 3s")
		}
	}
}

func jsonField(t *testing.T, data, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("decoding event %s: %v", data, err)
	}
	v, _ := m[key].(string)
	return v
}

func streamClient() *http.Client {
	// One connection per stream, so an ended stream really ends its socket and
	// the goroutine count can be checked.
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

func TestStreamDeliversTheChangesAsTheyHappen(t *testing.T) {
	fastStream(t, 5*time.Millisecond, time.Hour)
	_, h := testServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	c := openSSE(t, ts.URL, "/api/memory/stream", "", "", streamClient())

	first := remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces", "agent": "probe"})
	added := nextEvent(t, c)
	if added.Event != evAdded || jsonField(t, added.Data, "text") != "the user prefers spaces" {
		t.Fatalf("first event = %+v", added)
	}
	if jsonField(t, added.Data, "id") != first["id"] {
		t.Errorf("event id %q, remember returned %v", jsonField(t, added.Data, "id"), first["id"])
	}

	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs", "agent": "probe"})
	// A poll can land between the two index writes of a correction, so the new
	// fact may first show as added. The superseded event must still follow.
	var sup sseFrame
	for i := 0; i < 3 && sup.Event != evSuperseded; i++ {
		sup = nextEvent(t, c)
	}
	if sup.Event != evSuperseded {
		t.Fatalf("no superseded event after the correction: %+v", sup)
	}
	if jsonField(t, sup.Data, "text") != "the user prefers tabs" ||
		jsonField(t, sup.Data, "replaced_text") != "the user prefers spaces" ||
		jsonField(t, sup.Data, "replaces_id") != first["id"] {
		t.Errorf("superseded event does not carry both texts: %s", sup.Data)
	}

	successor := jsonField(t, sup.Data, "id")
	path := jsonField(t, sup.Data, "path")
	if w := do(t, h, "DELETE", "/api/memory/entry?path="+url.QueryEscape(path)+"&id="+successor, nil); w.Code >= 300 {
		t.Fatalf("forget = %d %s", w.Code, w.Body)
	}
	var forgot sseFrame
	for i := 0; i < 3 && forgot.Event != evForgotten; i++ {
		forgot = nextEvent(t, c)
	}
	if forgot.Event != evForgotten || jsonField(t, forgot.Data, "reason") != "retracted" {
		t.Fatalf("no retraction event: %+v", forgot)
	}
}

func TestStreamResumesFromLastEventID(t *testing.T) {
	fastStream(t, 5*time.Millisecond, time.Hour)
	_, h := testServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	c1 := openSSE(t, ts.URL, "/api/memory/stream", "", "", streamClient())
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces", "agent": "probe"})
	seen := nextEvent(t, c1)
	c1.close()

	// Written while nobody is listening.
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs", "agent": "probe"})

	c2 := openSSE(t, ts.URL, "/api/memory/stream", "", seen.ID, streamClient())
	// The replay may include what the client already had (the same minute is
	// replayed whole), so read until the correction appears, not a fixed count.
	found := false
	for i := 0; i < 4 && !found; i++ {
		f := nextEvent(t, c2)
		if f.Event == evSuperseded && jsonField(t, f.Data, "replaces_id") == jsonField(t, seen.Data, "id") {
			found = true
		}
	}
	if !found {
		t.Errorf("the correction written during the gap was not replayed")
	}
}

func TestStreamFiltersByAgent(t *testing.T) {
	fastStream(t, 5*time.Millisecond, time.Hour)
	_, h := testServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	c := openSSE(t, ts.URL, "/api/memory/stream?agent=alpha", "", "", streamClient())
	remember(t, h, map[string]any{"topic": "one", "text": "beta learned this", "agent": "beta"})
	remember(t, h, map[string]any{"topic": "two", "text": "alpha learned this", "agent": "alpha"})

	f := nextEvent(t, c)
	if jsonField(t, f.Data, "agent") != "alpha" || jsonField(t, f.Data, "text") != "alpha learned this" {
		t.Fatalf("agent filter let through %s", f.Data)
	}
}

func TestStreamNeverLeaksAFactTheCallerCannotRead(t *testing.T) {
	fastStream(t, 5*time.Millisecond, time.Hour)
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")
	alice, err := s.Auth.ByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	aliceStream := openSSE(t, ts.URL, "/api/memory/stream", adminKey, "", streamClient())
	bobStream := openSSE(t, ts.URL, "/api/memory/stream", bobKey, "", streamClient())

	// A fact in a note only alice may read. The memory line is written the way
	// the engine writes it, so the stream sees a real entry.
	stamp := time.Now().In(time.Local).Format(memory.StampFormat)
	const secret = "VAULT_SECRET the deploy key lives in the vault"
	body := memory.Append("", memory.Entry{ID: memory.DeriveID(stamp, "probe", secret),
		Text: secret, Agent: "probe", Stamp: stamp})
	if _, err := s.WriteNote("memory/vip.md", body, map[string]any{"readers": alice.ID}); err != nil {
		t.Fatal(err)
	}

	f := nextEvent(t, aliceStream)
	if !strings.Contains(f.Data, "VAULT_SECRET") {
		t.Fatalf("the owner should see her own private fact: %s", f.Data)
	}

	// A public fact, to prove bob's stream is live and has passed the private
	// one by. Anything before it on bob's stream would be a leak.
	if w := asKey(t, h, adminKey, "POST", "/api/memory", map[string]any{
		"topic": "public", "text": "the office is on floor seven", "agent": "probe"}); w.Code >= 300 {
		t.Fatalf("remember = %d %s", w.Code, w.Body)
	}
	pub := nextEvent(t, bobStream)
	if !strings.Contains(pub.Data, "floor seven") {
		t.Fatalf("bob's first event = %s", pub.Data)
	}
	if strings.Contains(pub.Data, "VAULT_SECRET") || strings.Contains(pub.Data, "vip.md") {
		t.Fatalf("private fact leaked to bob: %s", pub.Data)
	}
}

func TestStreamSendsHeartbeats(t *testing.T) {
	fastStream(t, time.Hour, 10*time.Millisecond)
	_, h := testServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	c := openSSE(t, ts.URL, "/api/memory/stream", "", "", streamClient())
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				t.Fatal("stream ended")
			}
			if strings.HasPrefix(f.Comment, "heartbeat") {
				return
			}
		case <-deadline:
			t.Fatal("no heartbeat within 2s")
		}
	}
}

func TestStreamGoroutinesEndWhenTheClientLeaves(t *testing.T) {
	fastStream(t, 2*time.Millisecond, time.Hour)
	_, h := testServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// Warm up once, so lazily started runtime goroutines are not counted as a leak.
	warm := openSSE(t, ts.URL, "/api/memory/stream", "", "", streamClient())
	warm.close()
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		c := openSSE(t, ts.URL, "/api/memory/stream", "", "", streamClient())
		time.Sleep(10 * time.Millisecond)
		c.close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Fatalf("goroutines did not return after five disconnects: before %d, after %d", before, n)
	}
}

func TestServerCloseEndsOpenStreams(t *testing.T) {
	fastStream(t, time.Hour, time.Hour)
	s, h := testServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	c := openSSE(t, ts.URL, "/api/memory/stream", "", "", streamClient())
	s.Close()
	s.Close() // idempotent
	select {
	case _, ok := <-c.frames:
		for ok {
			_, ok = <-c.frames
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an open stream outlived Server.Close")
	}
}

// advance is the whole of the delivery rule, so it is tested directly: a late
// same-minute event is still delivered, a sent one never is, and older minutes
// never are.
func TestAdvanceDeliversLateEventsOfTheSameMinute(t *testing.T) {
	t.Parallel()
	ev := func(at, kind, id string) memEvent {
		return memEvent{At: at, Event: kind, ID: id, Key: eventKey(at, kind, id)}
	}
	const m = "2026-10-10 14:03"
	pos := streamPos{minute: m, sent: map[string]bool{}}

	b := ev(m, evSuperseded, "bbb")
	out, pos := advance([]memEvent{b}, pos)
	if len(out) != 1 || out[0].ID != "bbb" {
		t.Fatalf("first poll = %v", out)
	}

	// An earlier key in the same minute arrives after the later one.
	a := ev(m, evAdded, "aaa")
	out, pos = advance([]memEvent{a, b}, pos)
	if len(out) != 1 || out[0].ID != "aaa" {
		t.Fatalf("late same-minute event was dropped or the sent one repeated: %v", out)
	}

	// A newer minute moves the position on, and the old minute is not revisited.
	c := ev("2026-10-10 14:04", evForgotten, "ccc")
	out, pos = advance([]memEvent{a, b, c}, pos)
	if len(out) != 1 || out[0].ID != "ccc" {
		t.Fatalf("new minute poll = %v", out)
	}
	old := ev("2026-10-10 14:02", evAdded, "old")
	out, _ = advance([]memEvent{old, a, b, c}, pos)
	if len(out) != 0 {
		t.Fatalf("an event from before the position was delivered: %v", out)
	}
}

func TestParseEventIDAcceptsOnlyWellFormedIDs(t *testing.T) {
	t.Parallel()
	good := eventKey("2026-10-10 14:03", evAdded, "abc123")
	if at, ok := parseEventID(good); !ok || at != "2026-10-10 14:03" {
		t.Errorf("parseEventID(%q) = %q, %v", good, at, ok)
	}
	for _, bad := range []string{"", "nonsense", "2026-10-10 14:03|memory.added", "not a time|memory.added|x"} {
		if _, ok := parseEventID(bad); ok {
			t.Errorf("parseEventID(%q) accepted a malformed id", bad)
		}
	}
}
