package connectors

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A tiny IMAP server: just enough of the protocol for the client to run
// against, speaking real framing (tagged replies, {n} literals) over a real
// socket. It records every command so tests can assert the client never did
// anything but read.

type fakeIMAPMsg struct {
	uid  uint32
	date time.Time
	raw  string
}

type fakeIMAPFolder struct {
	validity uint32
	msgs     []fakeIMAPMsg
}

type fakeIMAP struct {
	t        *testing.T
	ln       net.Listener
	mu       sync.Mutex
	folders  map[string]*fakeIMAPFolder
	password string
	cmds     []string
	failEcho bool // reject LOGIN with a message that echoes the password
}

func startFakeIMAP(t *testing.T, password string) *fakeIMAP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIMAP{t: t, ln: ln, password: password, folders: map[string]*fakeIMAPFolder{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeIMAP) port() string {
	_, p, _ := net.SplitHostPort(f.ln.Addr().String())
	return p
}

func (f *fakeIMAP) add(folder string, uid uint32, date string, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fo := f.folders[folder]
	if fo == nil {
		fo = &fakeIMAPFolder{validity: 100}
		f.folders[folder] = fo
	}
	d, _ := time.Parse("2006-01-02", date)
	fo.msgs = append(fo.msgs, fakeIMAPMsg{uid, d, strings.ReplaceAll(raw, "\n", "\r\n")})
}

func (f *fakeIMAP) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.cmds...)
}

func (f *fakeIMAP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(s string) { fmt.Fprint(c, s) }
	w("* OK fake imap ready\r\n")
	var folder *fakeIMAPFolder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		tag, rest, _ := strings.Cut(line, " ")
		verb, args, _ := strings.Cut(rest, " ")
		verb = strings.ToUpper(verb)
		f.mu.Lock()
		if verb != "LOGIN" {
			f.cmds = append(f.cmds, rest)
		} else {
			f.cmds = append(f.cmds, "LOGIN")
		}
		f.mu.Unlock()
		switch verb {
		case "LOGIN":
			if !strings.Contains(args, `"`+f.password+`"`) {
				if f.failEcho {
					w(tag + " NO [AUTHENTICATIONFAILED] bad login: " + args + "\r\n")
				} else {
					w(tag + " NO [AUTHENTICATIONFAILED] invalid credentials\r\n")
				}
				continue
			}
			w(tag + " OK logged in\r\n")
		case "EXAMINE", "SELECT":
			name := strings.Trim(args, `"`)
			f.mu.Lock()
			fo := f.folders[name]
			f.mu.Unlock()
			if fo == nil {
				w(tag + " NO no such mailbox\r\n")
				continue
			}
			folder = fo
			f.mu.Lock()
			v := fo.validity
			f.mu.Unlock()
			w(fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY %d] ok\r\n%s OK [READ-ONLY] done\r\n", len(fo.msgs), v, tag))
		case "UID":
			sub, a, _ := strings.Cut(args, " ")
			switch strings.ToUpper(sub) {
			case "SEARCH":
				f.mu.Lock()
				toks := fakeTokens(a)
				pred, _ := fakeParse(toks)
				var hits []string
				var max uint32
				for _, m := range folder.msgs {
					if m.uid > max {
						max = m.uid
					}
				}
				_ = max
				for _, m := range folder.msgs {
					if pred(m, max) {
						hits = append(hits, strconv.Itoa(int(m.uid)))
					}
				}
				f.mu.Unlock()
				w("* SEARCH " + strings.Join(hits, " ") + "\r\n" + tag + " OK done\r\n")
			case "FETCH":
				set, _, _ := strings.Cut(a, " ")
				want := map[uint32]bool{}
				for _, s := range strings.Split(set, ",") {
					n, _ := strconv.Atoi(s)
					want[uint32(n)] = true
				}
				f.mu.Lock()
				for i, m := range folder.msgs {
					if want[m.uid] {
						fmt.Fprintf(c, "* %d FETCH (BODY[] {%d}\r\n%s UID %d)\r\n", i+1, len(m.raw), m.raw, m.uid)
					}
				}
				f.mu.Unlock()
				w(tag + " OK done\r\n")
			default:
				w(tag + " BAD unsupported\r\n")
			}
		case "LOGOUT":
			w("* BYE bye\r\n" + tag + " OK done\r\n")
			return
		default:
			w(tag + " BAD unsupported\r\n")
		}
	}
}

func fakeTokens(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		switch {
		case s[i] == ' ':
			i++
		case s[i] == '"':
			j := i + 1
			var b strings.Builder
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
				j++
			}
			out = append(out, "\x00"+b.String())
			i = j + 1
		default:
			j := i
			for j < len(s) && s[j] != ' ' {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out
}

type fakePred func(m fakeIMAPMsg, max uint32) bool

// fakeParse parses an AND-sequence of search keys.
func fakeParse(toks []string) (fakePred, []string) {
	var preds []fakePred
	for len(toks) > 0 {
		var p fakePred
		p, toks = fakeKey(toks)
		preds = append(preds, p)
	}
	return func(m fakeIMAPMsg, max uint32) bool {
		for _, p := range preds {
			if !p(m, max) {
				return false
			}
		}
		return true
	}, nil
}

func fakeKey(toks []string) (fakePred, []string) {
	if toks[0] == "CHARSET" {
		toks = toks[2:]
	}
	key := strings.ToUpper(toks[0])
	val := func(s string) string { return strings.TrimPrefix(s, "\x00") }
	switch key {
	case "OR":
		a, rest := fakeKey(toks[1:])
		b, rest := fakeKey(rest)
		return func(m fakeIMAPMsg, mx uint32) bool { return a(m, mx) || b(m, mx) }, rest
	case "SINCE":
		d, _ := time.Parse("02-Jan-2006", toks[1])
		return func(m fakeIMAPMsg, _ uint32) bool { return !m.date.Before(d) }, toks[2:]
	case "UID":
		lo, hi, _ := strings.Cut(toks[1], ":")
		l, _ := strconv.Atoi(lo)
		return func(m fakeIMAPMsg, mx uint32) bool {
			h := int(mx)
			if hi != "*" {
				h, _ = strconv.Atoi(hi)
			}
			// IMAP quirk: "n:*" always includes the last message.
			if hi == "*" && int(m.uid) == int(mx) {
				return true
			}
			return int(m.uid) >= l && int(m.uid) <= h
		}, toks[2:]
	case "TEXT":
		q := strings.ToLower(val(toks[1]))
		return func(m fakeIMAPMsg, _ uint32) bool { return strings.Contains(strings.ToLower(m.raw), q) }, toks[2:]
	case "SUBJECT":
		q := strings.ToLower(val(toks[1]))
		return func(m fakeIMAPMsg, _ uint32) bool {
			return strings.Contains(strings.ToLower(fakeHeader(m.raw, "Subject")), q)
		}, toks[2:]
	case "HEADER":
		name, q := toks[1], strings.ToLower(val(toks[2]))
		return func(m fakeIMAPMsg, _ uint32) bool {
			return strings.Contains(strings.ToLower(fakeHeader(m.raw, name)), q)
		}, toks[3:]
	}
	return func(fakeIMAPMsg, uint32) bool { return false }, toks[1:]
}

func fakeHeader(raw, name string) string {
	head, _, _ := strings.Cut(raw, "\r\n\r\n")
	for _, l := range strings.Split(head, "\r\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---- canned mail -----------------------------------------------------------

const imapUser = "me@example.com"
const imapPass = "hunter2-app-password"

var imapPDF = base64.StdEncoding.EncodeToString([]byte("SECRET-PDF-CONTENT-DO-NOT-INDEX"))

func seedFakeIMAP(f *fakeIMAP) {
	f.add("INBOX", 1, "2026-10-01", `Message-ID: <m1@x>
From: Alice <alice@example.org>
To: me@example.com
Date: Thu, 01 Oct 2026 09:00:00 +0000
Subject: Launch plan
Content-Type: text/plain; charset=utf-8

We should launch on Friday.
`)
	f.add("INBOX", 2, "2026-10-02", `Message-ID: <m2@x>
In-Reply-To: <m1@x>
References: <m1@x>
From: Bob <bob@example.org>
To: me@example.com, alice@example.org
Date: Fri, 02 Oct 2026 09:00:00 +0000
Subject: Re: Launch plan
Content-Type: text/plain

Friday works for me.
`)
	f.add("INBOX", 3, "2026-10-03", `Message-ID: <m3@x>
From: Carol <carol@example.org>
To: me@example.com
Date: Sat, 03 Oct 2026 09:00:00 +0000
Subject: Quarterly report
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="BOUND"

--BOUND
Content-Type: text/plain

Report attached.
--BOUND
Content-Type: application/pdf; name="report.pdf"
Content-Disposition: attachment; filename="report.pdf"
Content-Transfer-Encoding: base64

`+imapPDF+`
--BOUND--
`)
	f.add("INBOX", 4, "2026-10-04", `Message-ID: <m4@x>
From: =?UTF-8?Q?Ren=C3=A9?= <rene@example.org>
To: me@example.com
Date: Sun, 04 Oct 2026 09:00:00 +0000
Subject: =?UTF-8?Q?Caf=C3=A9_menu?=
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

caf=C3=A9 costs 3 =3D three euros.
`)
	f.add("INBOX", 5, "2026-10-05", `Message-ID: <m5@x>
From: News <news@example.org>
To: me@example.com
Date: Mon, 05 Oct 2026 09:00:00 +0000
Subject: Newsletter
Content-Type: text/html; charset=utf-8

<html><body><p>Hello <b>html</b> world</p></body></html>
`)
	f.add("Sent", 1, "2026-10-02", `Message-ID: <s1@x>
In-Reply-To: <m2@x>
References: <m1@x> <m2@x>
From: Me <me@example.com>
To: alice@example.org
Date: Fri, 02 Oct 2026 12:00:00 +0000
Subject: Re: Launch plan
Content-Type: text/plain

Great, Friday it is.
`)
	f.add("Sent", 2, "2026-10-06", `Message-ID: <s2@x>
From: Me <me@example.com>
To: dana@example.org
Date: Tue, 06 Oct 2026 12:00:00 +0000
Subject: Lunch?
Content-Type: text/plain

Lunch on Thursday?
`)
}

func imapInput(f *fakeIMAP, folders, cursor string) Input {
	cfg := Config{"host": "127.0.0.1", "port": f.port(), "security": "none",
		"username": imapUser, "folders": folders}
	return Input{Config: cfg, Secret: imapPass, Cursor: cursor, Limit: 50}
}

func withClock(t *testing.T) {
	old := timeNow
	timeNow = func() time.Time { return time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { timeNow = old })
}

func docByTitle(t *testing.T, docs []Document, title string) Document {
	t.Helper()
	for _, d := range docs {
		if d.Title == title {
			return d
		}
	}
	t.Fatalf("no document titled %q among %d docs", title, len(docs))
	return Document{}
}

func noSecret(t *testing.T, docs []Document) {
	t.Helper()
	for _, d := range docs {
		blob := d.Title + d.Body + d.ExternalID + d.Author + fmt.Sprint(d.Meta)
		if strings.Contains(blob, imapPass) {
			t.Fatalf("password leaked into document %q", d.Title)
		}
	}
}

// ---- tests -----------------------------------------------------------------

func TestIMAPFirstSyncGroupsThreadsAndListsAttachments(t *testing.T) {
	withClock(t)
	f := startFakeIMAP(t, imapPass)
	seedFakeIMAP(f)
	src, _ := Get("imap")
	page, err := src.Fetch(context.Background(), imapInput(f, "INBOX, Sent", ""))
	if err != nil {
		t.Fatal(err)
	}
	// threads: launch (3 msgs across two folders), report, cafe, newsletter, lunch
	if len(page.Docs) != 5 {
		t.Fatalf("got %d documents, want 5 (one per thread)", len(page.Docs))
	}
	launch := docByTitle(t, page.Docs, "Launch plan")
	if launch.Meta["message_count"] != "3" {
		t.Errorf("launch thread has %s messages, want 3 (two inbox + one sent)", launch.Meta["message_count"])
	}
	if !strings.Contains(launch.Body, "launch on Friday") || !strings.Contains(launch.Body, "Friday it is") {
		t.Errorf("thread body is missing messages:\n%s", launch.Body)
	}
	if launch.Meta["source"] != "imap" || !strings.HasPrefix(launch.ExternalID, "imap:") {
		t.Errorf("meta/external id wrong: %v %q", launch.Meta, launch.ExternalID)
	}
	if launch.Own {
		t.Error("a thread with other people's messages must not be Own")
	}

	rep := docByTitle(t, page.Docs, "Quarterly report")
	if !strings.Contains(rep.Body, "report.pdf (application/pdf, 31 bytes)") {
		t.Errorf("attachment not listed with type and size:\n%s", rep.Body)
	}
	if strings.Contains(rep.Body, "SECRET-PDF-CONTENT") || strings.Contains(rep.Body, imapPDF) {
		t.Error("attachment content was ingested")
	}

	cafe := docByTitle(t, page.Docs, "Café menu")
	if !strings.Contains(cafe.Body, "café costs 3 = three euros.") {
		t.Errorf("quoted-printable/encoded-word not decoded:\n%s", cafe.Body)
	}
	news := docByTitle(t, page.Docs, "Newsletter")
	if strings.Contains(news.Body, "<p>") || !strings.Contains(news.Body, "html") {
		t.Errorf("html-only message not reduced to text:\n%s", news.Body)
	}
	lunch := docByTitle(t, page.Docs, "Lunch?")
	if !lunch.Own {
		t.Error("a thread the owner alone wrote should be Own")
	}
	noSecret(t, page.Docs)

	if page.Cursor != "INBOX=100:5;Sent=100:2" {
		t.Errorf("cursor = %q", page.Cursor)
	}
	// Read-only: nothing but these commands may ever have been sent.
	for _, c := range f.commands() {
		v := strings.ToUpper(strings.Fields(c)[0])
		switch v {
		case "LOGIN", "EXAMINE", "LOGOUT":
		case "UID":
			if s := strings.ToUpper(strings.Fields(c)[1]); s != "SEARCH" && s != "FETCH" {
				t.Errorf("unexpected command %q", c)
			}
			if strings.Contains(strings.ToUpper(c), "BODY[") && !strings.Contains(strings.ToUpper(c), "PEEK") {
				t.Errorf("non-PEEK fetch would mark mail read: %q", c)
			}
		default:
			t.Errorf("unexpected (non-read-only) command %q", c)
		}
	}
}

func TestIMAPIncrementalReturnsOnlyNewThreadActivityWithFullThread(t *testing.T) {
	withClock(t)
	f := startFakeIMAP(t, imapPass)
	seedFakeIMAP(f)
	src, _ := Get("imap")
	first, err := src.Fetch(context.Background(), imapInput(f, "INBOX, Sent", ""))
	if err != nil {
		t.Fatal(err)
	}
	again, err := src.Fetch(context.Background(), imapInput(f, "INBOX, Sent", first.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Docs) != 0 {
		t.Fatalf("nothing changed but got %d documents", len(again.Docs))
	}
	if again.Cursor != first.Cursor {
		t.Errorf("cursor moved with no new mail: %q -> %q", first.Cursor, again.Cursor)
	}

	f.add("INBOX", 6, "2026-10-08", `Message-ID: <m6@x>
In-Reply-To: <m2@x>
References: <m1@x> <m2@x>
From: Alice <alice@example.org>
To: me@example.com
Date: Thu, 08 Oct 2026 09:00:00 +0000
Subject: Re: Launch plan
Content-Type: text/plain

Confirmed, see you Friday.
`)
	next, err := src.Fetch(context.Background(), imapInput(f, "INBOX, Sent", again.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Docs) != 1 {
		t.Fatalf("got %d documents, want only the thread with new activity", len(next.Docs))
	}
	d := next.Docs[0]
	if d.Title != "Launch plan" || d.Meta["message_count"] != "4" {
		t.Errorf("want the whole 4-message thread, got %q with %s messages", d.Title, d.Meta["message_count"])
	}
	for _, want := range []string{"launch on Friday", "Friday works", "Friday it is", "Confirmed, see you"} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("full thread is missing %q", want)
		}
	}
	if d.ExternalID != docByTitle(t, first.Docs, "Launch plan").ExternalID {
		t.Error("external id changed between syncs, so the update would be a duplicate")
	}
	if next.Cursor != "INBOX=100:6;Sent=100:2" {
		t.Errorf("cursor = %q", next.Cursor)
	}
}

func TestIMAPRefusesCleartextToARemoteHost(t *testing.T) {
	src, _ := Get("imap")
	for _, host := range []string{"mail.example.com", "10.0.0.5"} {
		_, err := src.Fetch(context.Background(), Input{
			Config: Config{"host": host, "security": "none", "username": "u"},
			Secret: imapPass, Limit: 5,
		})
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", host, err)
		}
		if err != nil && strings.Contains(err.Error(), imapPass) {
			t.Error("password in error")
		}
	}
	_, err := src.Fetch(context.Background(), Input{
		Config: Config{"host": "h", "security": "tls?", "username": "u"}, Secret: "x",
	})
	if !errors.Is(err, ErrConfig) {
		t.Errorf("bad security: %v", err)
	}
	if err := Validate("imap", Config{"username": "u"}); !errors.Is(err, ErrConfig) {
		t.Errorf("missing host not rejected: %v", err)
	}
}

func TestIMAPSearchNewestFirst(t *testing.T) {
	withClock(t)
	f := startFakeIMAP(t, imapPass)
	seedFakeIMAP(f)
	src, _ := Get("imap")
	hits, err := src.(Searcher).Search(context.Background(), imapInput(f, "INBOX, Sent", ""), `friday "x"`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("quote in query should match nothing, got %v", hits)
	}
	hits, err = src.(Searcher).Search(context.Background(), imapInput(f, "INBOX, Sent", ""), "Friday", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3: %+v", len(hits), hits)
	}
	if hits[0].ID != "Sent|1" || hits[2].ID != "INBOX|1" {
		t.Errorf("not newest-first: %+v", hits)
	}
	if hits[0].Title != "Re: Launch plan" || !strings.Contains(hits[0].Snippet, "Friday it is") {
		t.Errorf("hit = %+v", hits[0])
	}
	if one, _ := src.(Searcher).Search(context.Background(), imapInput(f, "INBOX, Sent", ""), "Friday", 1); len(one) != 1 {
		t.Errorf("limit not applied: %d", len(one))
	}
	if _, err := src.(Searcher).Search(context.Background(), imapInput(f, "INBOX", ""), "a\r\nb", 5); !errors.Is(err, ErrConfig) {
		t.Errorf("CRLF in query accepted: %v", err)
	}
}

func TestIMAPReadReturnsWholeThread(t *testing.T) {
	withClock(t)
	f := startFakeIMAP(t, imapPass)
	seedFakeIMAP(f)
	src, _ := Get("imap")
	item, err := src.(Reader).Read(context.Background(), imapInput(f, "INBOX, Sent", ""), "INBOX|2")
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Launch plan" || !strings.Contains(item.Body, "launch on Friday") ||
		!strings.Contains(item.Body, "Friday it is") {
		t.Errorf("not the whole thread:\n%s", item.Body)
	}
	rep, err := src.(Reader).Read(context.Background(), imapInput(f, "INBOX", ""), "INBOX|3")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Attachments) != 1 || rep.Attachments[0] != "report.pdf (application/pdf, 31 bytes)" {
		t.Errorf("attachments = %v", rep.Attachments)
	}
	if strings.Contains(rep.Body, "SECRET-PDF") {
		t.Error("attachment content in body")
	}
	if _, err := src.(Reader).Read(context.Background(), imapInput(f, "INBOX", ""), "INBOX|99"); err == nil {
		t.Error("missing message should error")
	}
	if _, err := src.(Reader).Read(context.Background(), imapInput(f, "INBOX", ""), "garbage"); err == nil {
		t.Error("bad id should error")
	}
}

func TestIMAPUIDValidityChangeRestartsFolder(t *testing.T) {
	withClock(t)
	f := startFakeIMAP(t, imapPass)
	seedFakeIMAP(f)
	src, _ := Get("imap")
	first, err := src.Fetch(context.Background(), imapInput(f, "INBOX", ""))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.folders["INBOX"].validity = 200
	f.mu.Unlock()
	again, err := src.Fetch(context.Background(), imapInput(f, "INBOX", first.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Docs) != 4 {
		t.Fatalf("after UIDVALIDITY change got %d docs, want a full restart (4)", len(again.Docs))
	}
	if again.Cursor != "INBOX=200:5" {
		t.Errorf("cursor = %q", again.Cursor)
	}
	// Same ids as before, so the restart updates notes rather than duplicating.
	if docByTitle(t, again.Docs, "Launch plan").ExternalID != docByTitle(t, first.Docs, "Launch plan").ExternalID {
		t.Error("external id differs after restart")
	}
}

func TestIMAPSinceLimitsFirstSync(t *testing.T) {
	withClock(t)
	f := startFakeIMAP(t, imapPass)
	seedFakeIMAP(f)
	in := imapInput(f, "INBOX", "")
	in.Config["since"] = "2026-10-04"
	src, _ := Get("imap")
	page, err := src.Fetch(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Docs) != 2 {
		t.Fatalf("got %d docs, want the two messages on/after the cutoff", len(page.Docs))
	}
}

func TestIMAPLimitAdvancesCursorOnlyOverReturnedMessages(t *testing.T) {
	withClock(t)
	f := startFakeIMAP(t, imapPass)
	seedFakeIMAP(f)
	src, _ := Get("imap")
	in := imapInput(f, "INBOX", "")
	in.Limit = 2
	var titles []string
	for i := 0; i < 5; i++ {
		page, err := src.Fetch(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range page.Docs {
			titles = append(titles, d.Title)
		}
		in.Cursor = page.Cursor
		if !page.More {
			break
		}
		if len(page.Docs) != 2 {
			t.Fatalf("More with %d docs", len(page.Docs))
		}
	}
	if len(titles) != 4 {
		t.Fatalf("paging returned %v, want all 4 threads exactly once", titles)
	}
}

func TestIMAPPasswordNeverAppearsInErrors(t *testing.T) {
	f := startFakeIMAP(t, "different-password")
	f.failEcho = true
	seedFakeIMAP(f)
	src, _ := Get("imap")
	_, err := src.Fetch(context.Background(), imapInput(f, "INBOX", ""))
	if err == nil {
		t.Fatal("login with the wrong password succeeded")
	}
	if strings.Contains(err.Error(), imapPass) {
		t.Errorf("password leaked in error: %v", err)
	}
	for _, e := range []error{
		func() error {
			_, e := src.(Searcher).Search(context.Background(), imapInput(f, "INBOX", ""), "x", 5)
			return e
		}(),
		func() error {
			_, e := src.(Reader).Read(context.Background(), imapInput(f, "INBOX", ""), "INBOX|1")
			return e
		}(),
	} {
		if e == nil || strings.Contains(e.Error(), imapPass) {
			t.Errorf("bad error: %v", e)
		}
	}
	// A missing folder reports the folder, not the credential.
	g := startFakeIMAP(t, imapPass)
	_, err = src.Fetch(context.Background(), imapInput(g, "Nope", ""))
	if err == nil || !strings.Contains(err.Error(), "Nope") || strings.Contains(err.Error(), imapPass) {
		t.Errorf("missing folder error: %v", err)
	}
}

func TestIMAPDescribeExplainsAppPasswords(t *testing.T) {
	k := imapSource{}.Describe()
	if k.DefaultPrefix != "connectors/mail/imap" || !strings.Contains(strings.ToLower(k.SecretHelp), "app") ||
		!strings.Contains(k.SecretHelp, "Gmail") {
		t.Errorf("describe = %+v", k)
	}
	if _, ok := Source(imapSource{}).(Actor); ok {
		t.Error("imap must not be an Actor")
	}
}
