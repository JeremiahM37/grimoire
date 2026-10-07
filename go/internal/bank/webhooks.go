package bank

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// Webhooks tell another system that a bank changed, so it need not poll.
//
// A webhook is registered for one bank or, by an administrator, for every
// bank (bank ""). When an event fires, one delivery row per matching webhook
// is written first and sent after, so a delivery survives a restart and its
// attempts are on record. A failed delivery is retried 5 s, 5 min, 30 min,
// 2 h and 5 h after successive failures, then given up.
//
// Each request is signed with the webhook's secret: X-Grimoire-Signature is
// "t=<unix seconds>,v1=<hex HMAC-SHA256 of '<t>.<body>'>", re-signed on every
// attempt so a receiver can refuse a replayed one, and X-Grimoire-Signature-256
// is "sha256=<hex HMAC-SHA256 of the body>" for receivers that only know the
// GitHub-style header.
//
// The URL comes from whoever registers it, so it is checked twice against
// internal addresses: when registered (every address the name resolves to),
// and when connecting (the address the socket actually uses), which also
// covers DNS rebinding and redirects. Loopback and private networks are
// refused unless the operator allows them (webhook_allow_private); link-local
// and cloud metadata addresses are refused always.

// Event names.
const (
	EventRetainCompleted        = "retain.completed"
	EventConsolidationCompleted = "consolidation.completed"
	EventReflectCompleted       = "reflect.completed"
)

var knownEvents = map[string]bool{EventRetainCompleted: true, EventConsolidationCompleted: true,
	EventReflectCompleted: true, "*": true}

// DefaultWebhookDelays are the waits before each retry.
var DefaultWebhookDelays = []time.Duration{5 * time.Second, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 5 * time.Hour}

// Webhook is one registration.
type Webhook struct {
	ID     string   `json:"id"`
	BankID string   `json:"bank_id"` // "" for every bank
	URL    string   `json:"url"`
	Events []string `json:"events"`
	// Secret is shown once, in the response that created it.
	Secret    string `json:"secret,omitempty"`
	HasSecret bool   `json:"has_secret"`
	Enabled   bool   `json:"enabled"`
	Created   string `json:"created_at"`
	Updated   string `json:"updated_at,omitempty"`
}

// WebhookSpec creates or patches a webhook. Nil is "not sent".
type WebhookSpec struct {
	URL     *string   `json:"url"`
	Secret  *string   `json:"secret"`
	Events  *[]string `json:"events"`
	Enabled *bool     `json:"enabled"`
}

// Delivery is one event sent (or being sent) to one webhook.
type Delivery struct {
	ID         string `json:"id"`
	WebhookID  string `json:"webhook_id"`
	BankID     string `json:"bank_id"`
	Event      string `json:"event"`
	Status     string `json:"status"` // pending | delivered | failed
	Attempts   int    `json:"attempts"`
	NextAt     string `json:"next_attempt_at,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	LastStatus int    `json:"last_response_status,omitempty"`
	Created    string `json:"created_at"`
	Updated    string `json:"updated_at,omitempty"`
	Payload    string `json:"payload,omitempty"`
}

func (e *Engine) allowPrivate() bool {
	return e.AllowPrivateWebhooks != nil && e.AllowPrivateWebhooks()
}

// ValidateWebhookURL refuses a URL that is not http(s) or that names an
// address a webhook must not reach.
func ValidateWebhookURL(ctx context.Context, raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return invalid("webhook url must be an absolute http or https URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return invalid("webhook url must be http or https")
	}
	if u.User != nil {
		return invalid("webhook url must not carry credentials")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if err := secrets.CheckIP(ip, allowPrivate); err != nil {
			return invalid("webhook url: %v", err)
		}
		return nil
	}
	// A host made only of digits, dots and hex is a numeric address in a
	// spelling net.ParseIP does not accept ("2130706433", "0x7f.1") — which
	// some resolvers would still turn into 127.0.0.1.
	if numericHost(host) {
		return invalid("webhook url: numeric host %q is not a standard address", host)
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	if err != nil || len(addrs) == 0 {
		return invalid("webhook url: cannot resolve %q", host)
	}
	for _, a := range addrs {
		if err := secrets.CheckIP(a.IP, allowPrivate); err != nil {
			return invalid("webhook url: %s resolves to a refused address: %v", host, err)
		}
	}
	return nil
}

func numericHost(h string) bool {
	h = strings.ToLower(h)
	if h == "" {
		return false
	}
	allDigits := true
	for _, r := range h {
		if !(r >= '0' && r <= '9' || r == '.') {
			allDigits = false
		}
	}
	if allDigits {
		return true
	}
	for _, label := range strings.Split(h, ".") {
		if strings.HasPrefix(label, "0x") {
			return true
		}
	}
	return false
}

func newSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "whsec_" + hex.EncodeToString(b)
}

func cleanEvents(evs []string) ([]string, error) {
	if len(evs) == 0 {
		return []string{EventRetainCompleted, EventConsolidationCompleted}, nil
	}
	out := unionTags(evs)
	for _, ev := range out {
		if !knownEvents[ev] {
			return nil, invalid("unknown event %q", ev)
		}
	}
	return out, nil
}

// CreateWebhook registers a webhook for bankID ("" for all banks).
func (e *Engine) CreateWebhook(ctx context.Context, bankID string, spec WebhookSpec) (*Webhook, error) {
	if spec.URL == nil {
		return nil, invalid("url is required")
	}
	if err := ValidateWebhookURL(ctx, *spec.URL, e.allowPrivate()); err != nil {
		return nil, err
	}
	evs, err := cleanEvents(derefList(spec.Events))
	if err != nil {
		return nil, err
	}
	secret := newSecret()
	if spec.Secret != nil && strings.TrimSpace(*spec.Secret) != "" {
		secret = strings.TrimSpace(*spec.Secret)
	}
	enabled := spec.Enabled == nil || *spec.Enabled
	id := "wh-" + hex.EncodeToString(randBytes(8))
	now := e.now().UnixMilli()
	if err := e.Index.DB.Exec("INSERT INTO bank_webhooks(id,bank,url,secret,events,enabled,created,updated) VALUES(?,?,?,?,?,?,?,?)",
		id, bankID, strings.TrimSpace(*spec.URL), secret, strings.Join(evs, ","), boolInt(enabled), now, now); err != nil {
		return nil, err
	}
	w, err := e.GetWebhook(bankID, id)
	if err != nil {
		return nil, err
	}
	w.Secret = secret
	return w, nil
}

func derefList(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanWebhook(sc interface{ Scan(...any) error }) (*Webhook, error) {
	var w Webhook
	var secret, events string
	var enabled int
	var created, updated int64
	if err := sc.Scan(&w.ID, &w.BankID, &w.URL, &secret, &events, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	w.HasSecret, w.Enabled = secret != "", enabled == 1
	w.Events = splitComma(events)
	if w.Events == nil {
		w.Events = []string{}
	}
	w.Created, w.Updated = msTime(created), msTime(updated)
	return &w, nil
}

const webhookColumns = "id,bank,url,secret,events,enabled,created,updated"

// GetWebhook returns one webhook registered on bankID.
func (e *Engine) GetWebhook(bankID, id string) (*Webhook, error) {
	w, err := scanWebhook(e.Index.DB.QueryRow("SELECT "+webhookColumns+" FROM bank_webhooks WHERE id=? AND bank=?", id, bankID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// ListWebhooks lists the webhooks registered on bankID ("" for global ones).
func (e *Engine) ListWebhooks(bankID string) ([]Webhook, error) {
	rows, err := e.Index.DB.Query("SELECT "+webhookColumns+" FROM bank_webhooks WHERE bank=? ORDER BY created", bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// UpdateWebhook patches a webhook.
func (e *Engine) UpdateWebhook(ctx context.Context, bankID, id string, spec WebhookSpec) (*Webhook, error) {
	w, err := e.GetWebhook(bankID, id)
	if err != nil {
		return nil, err
	}
	sets := []string{}
	args := []any{}
	if spec.URL != nil {
		if err := ValidateWebhookURL(ctx, *spec.URL, e.allowPrivate()); err != nil {
			return nil, err
		}
		sets, args = append(sets, "url=?"), append(args, strings.TrimSpace(*spec.URL))
	}
	if spec.Events != nil {
		evs, err := cleanEvents(*spec.Events)
		if err != nil {
			return nil, err
		}
		sets, args = append(sets, "events=?"), append(args, strings.Join(evs, ","))
	}
	if spec.Enabled != nil {
		sets, args = append(sets, "enabled=?"), append(args, boolInt(*spec.Enabled))
	}
	if spec.Secret != nil {
		sets, args = append(sets, "secret=?"), append(args, strings.TrimSpace(*spec.Secret))
	}
	if len(sets) == 0 {
		return w, nil
	}
	sets, args = append(sets, "updated=?"), append(args, e.now().UnixMilli())
	if err := e.Index.DB.Exec("UPDATE bank_webhooks SET "+strings.Join(sets, ",")+" WHERE id=? AND bank=?",
		append(args, id, bankID)...); err != nil {
		return nil, err
	}
	return e.GetWebhook(bankID, id)
}

// DeleteWebhook removes a webhook and its delivery log.
func (e *Engine) DeleteWebhook(bankID, id string) error {
	n, err := e.Index.DB.ExecAffected("DELETE FROM bank_webhooks WHERE id=? AND bank=?", id, bankID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return e.Index.DB.Exec("DELETE FROM bank_webhook_deliveries WHERE webhook=?", id)
}

// ListDeliveries lists a webhook's deliveries, newest first.
func (e *Engine) ListDeliveries(bankID, id string, limit int) ([]Delivery, error) {
	if _, err := e.GetWebhook(bankID, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := e.Index.DB.Query("SELECT id,webhook,bank,event,payload,status,attempts,next_at,last_error,last_status,created,updated"+
		" FROM bank_webhook_deliveries WHERE webhook=? ORDER BY created DESC, id LIMIT ?", id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var next, created, updated int64
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.BankID, &d.Event, &d.Payload, &d.Status, &d.Attempts, &next,
			&d.LastError, &d.LastStatus, &created, &updated); err != nil {
			return nil, err
		}
		if d.Status == "pending" {
			d.NextAt = msTime(next)
		}
		d.Created, d.Updated = msTime(created), msTime(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

// WebhookEvent is the JSON body a webhook receives.
type WebhookEvent struct {
	Event       string         `json:"event"`
	BankID      string         `json:"bank_id"`
	OperationID string         `json:"operation_id,omitempty"`
	Status      string         `json:"status"`
	Timestamp   string         `json:"timestamp"`
	Data        map[string]any `json:"data"`
}

// FireEvent records a delivery for every enabled webhook that wants the
// event, on this bank or on all banks. Delivery happens in the background.
func (e *Engine) FireEvent(bankID, event, opID, status string, data map[string]any) {
	rows, err := e.Index.DB.Query("SELECT id, events FROM bank_webhooks WHERE (bank=? OR bank='') AND enabled=1", bankID)
	if err != nil {
		log.Printf("banks: webhooks for %s: %v", bankID, err)
		return
	}
	var hooks []string
	for rows.Next() {
		var id, evs string
		if rows.Scan(&id, &evs) != nil {
			continue
		}
		for _, ev := range splitComma(evs) {
			if ev == event || ev == "*" {
				hooks = append(hooks, id)
				break
			}
		}
	}
	rows.Close()
	if len(hooks) == 0 {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	body, _ := json.Marshal(WebhookEvent{Event: event, BankID: bankID, OperationID: opID, Status: status,
		Timestamp: e.now().Format(time.RFC3339Nano), Data: data})
	now := e.now().UnixMilli()
	for _, h := range hooks {
		if err := e.Index.DB.Exec("INSERT INTO bank_webhook_deliveries(id,webhook,bank,event,payload,status,next_at,created,updated)"+
			" VALUES(?,?,?,?,?,?,?,?,?)", "dl-"+hex.EncodeToString(randBytes(8)), h, bankID, event, string(body),
			"pending", now, now, now); err != nil {
			log.Printf("banks: recording webhook delivery: %v", err)
		}
	}
	e.wakeDeliveries()
}

func (e *Engine) wakeDeliveries() {
	e.mu.Lock()
	ch := e.deliverWake
	e.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// SignWebhook is the v1 signature over a delivery: hex HMAC-SHA256 of
// "<unix seconds>.<body>" under the secret. Receivers recompute it to check a
// request came from this server and is not a replay of an old one.
func SignWebhook(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyWebhook checks an X-Grimoire-Signature header against a body,
// refusing a timestamp more than tolerance away from now.
func VerifyWebhook(secret, header string, body []byte, now time.Time, tolerance time.Duration) bool {
	var ts int64
	var sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sig = v
		}
	}
	if ts == 0 || sig == "" {
		return false
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(SignWebhook(secret, ts, body)))
}

func (e *Engine) webhookClient() *http.Client {
	if e.WebhookClient != nil {
		return e.WebhookClient
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: secrets.GuardedTransport(e.allowPrivate()),
		// A redirect is a second URL nobody validated at registration; the
		// dial guard would still check it, but there is no reason to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (e *Engine) webhookDelays() []time.Duration {
	if len(e.WebhookDelays) > 0 {
		return e.WebhookDelays
	}
	return DefaultWebhookDelays
}

// deliveryLoop sends due deliveries until stop closes.
func (e *Engine) deliveryLoop(stop chan struct{}) {
	e.mu.Lock()
	if e.deliverWake == nil {
		e.deliverWake = make(chan struct{}, 1)
	}
	wake := e.deliverWake
	e.mu.Unlock()
	for {
		n := e.DeliverDue(context.Background())
		if n > 0 {
			continue
		}
		select {
		case <-stop:
			return
		case <-wake:
		case <-time.After(time.Second):
		}
	}
}

type dueDelivery struct {
	id, hook, url, secret, event, payload string
	attempts                              int
}

// DeliverDue sends every delivery whose time has come, and reports how many
// it attempted.
func (e *Engine) DeliverDue(ctx context.Context) int {
	now := e.now().UnixMilli()
	rows, err := e.Index.DB.Query("SELECT d.id, d.webhook, w.url, w.secret, d.event, d.payload, d.attempts"+
		" FROM bank_webhook_deliveries d JOIN bank_webhooks w ON w.id=d.webhook"+
		" WHERE d.status='pending' AND d.next_at<=? ORDER BY d.next_at LIMIT 50", now)
	if err != nil {
		return 0
	}
	var due []dueDelivery
	for rows.Next() {
		var d dueDelivery
		if rows.Scan(&d.id, &d.hook, &d.url, &d.secret, &d.event, &d.payload, &d.attempts) == nil {
			due = append(due, d)
		}
	}
	rows.Close()
	client := e.webhookClient()
	for _, d := range due {
		e.deliver(ctx, client, d)
	}
	return len(due)
}

func (e *Engine) deliver(ctx context.Context, client *http.Client, d dueDelivery) {
	body := []byte(d.payload)
	status, errMsg := 0, ""
	ts := e.now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "grimoire-webhooks/1")
		req.Header.Set("X-Grimoire-Event", d.event)
		req.Header.Set("X-Grimoire-Delivery", d.id)
		req.Header.Set("X-Grimoire-Timestamp", strconv.FormatInt(ts, 10))
		if d.secret != "" {
			req.Header.Set("X-Grimoire-Signature", fmt.Sprintf("t=%d,v1=%s", ts, SignWebhook(d.secret, ts, body)))
			m := hmac.New(sha256.New, []byte(d.secret))
			m.Write(body)
			req.Header.Set("X-Grimoire-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			status = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if status < 200 || status >= 300 {
				errMsg = fmt.Sprintf("receiver answered %d", status)
			}
		}
	}
	if err != nil {
		errMsg = err.Error()
	}
	attempts := d.attempts + 1
	now := e.now()
	if errMsg == "" {
		_ = e.Index.DB.Exec("UPDATE bank_webhook_deliveries SET status='delivered', attempts=?, last_status=?, last_error='', updated=? WHERE id=?",
			attempts, status, now.UnixMilli(), d.id)
		return
	}
	delays := e.webhookDelays()
	// A refused address is not going to become acceptable by waiting.
	permanent := strings.Contains(errMsg, "refusing")
	if attempts > len(delays) || permanent {
		_ = e.Index.DB.Exec("UPDATE bank_webhook_deliveries SET status='failed', attempts=?, last_status=?, last_error=?, updated=? WHERE id=?",
			attempts, status, runeCut(errMsg, 1000), now.UnixMilli(), d.id)
		return
	}
	next := now.Add(delays[attempts-1]).UnixMilli()
	_ = e.Index.DB.Exec("UPDATE bank_webhook_deliveries SET attempts=?, next_at=?, last_status=?, last_error=?, updated=? WHERE id=?",
		attempts, next, status, runeCut(errMsg, 1000), now.UnixMilli(), d.id)
}
