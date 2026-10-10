package connectors

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
)

// Generic IMAP mail source.
//
// A hand-rolled, deliberately small IMAP4rev1 client: LOGIN, EXAMINE (never
// SELECT — this connector cannot modify a mailbox, and says so by only ever
// issuing read-only commands), UID SEARCH, UID FETCH BODY.PEEK[] and LOGOUT.
// It is stdlib-only on purpose; the protocol subset is tiny and a mail library
// would be a large dependency for the sake of five commands.
//
// One document per thread, via ThreadDocument. Attachments are listed and
// counted, never decoded into the document.

func init() { Register(imapSource{}) }

type imapSource struct{}

func (imapSource) Kind() string { return "imap" }

func (imapSource) Describe() Kind {
	return Kind{
		Kind: "imap",
		Name: "Mail (IMAP)",
		Help: "Reads mail from any IMAP server, read-only (folders are opened with " +
			"EXAMINE; nothing is flagged, moved or deleted). One note per conversation. " +
			"Attachments are listed by name and size and never ingested. Connections " +
			"use TLS; unencrypted IMAP is refused unless the server is on this machine.",
		SecretHelp: "An APP PASSWORD, not your account password. Gmail, iCloud and " +
			"Fastmail (and most providers with two-factor sign-in) require an " +
			"app-specific password for IMAP: create one in the provider's security " +
			"settings and paste it here. The real account password should not be used.",
		Fields: []Field{
			{Name: "host", Label: "IMAP host", Required: true, Placeholder: "imap.fastmail.com"},
			{Name: "port", Label: "Port", Placeholder: "993",
				Help: "Default 993 for ssl, 143 for starttls."},
			{Name: "security", Label: "Security", Placeholder: "ssl",
				Help: "ssl (default, implicit TLS), starttls, or none. none sends the " +
					"password unencrypted and is only accepted for localhost."},
			{Name: "username", Label: "Username", Required: true,
				Placeholder: "you@example.com"},
			{Name: "folders", Label: "Folders", Placeholder: "INBOX, Sent",
				Help: "Comma-separated. Default INBOX. Add the sent folder to capture " +
					"your own replies."},
			{Name: "since", Label: "Since", Placeholder: "2026-01-01",
				Help: "YYYY-MM-DD; first sync starts here. Default: the last 30 days."},
			{Name: "max_threads", Label: "Max threads per sync", Placeholder: "50"},
		},
		DefaultPrefix: "connectors/mail/imap",
	}
}

// ---- configuration ---------------------------------------------------------

type imapConfig struct {
	host, port, security, user string
	folders                    []string
	maxThreads                 int
}

func parseIMAPConfig(cfg Config) (imapConfig, error) {
	c := imapConfig{
		host:     cfg.Get("host"),
		port:     cfg.Get("port"),
		security: strings.ToLower(cfg.Get("security")),
		user:     cfg.Get("username"),
	}
	if c.host == "" {
		return c, missing("IMAP host")
	}
	if c.user == "" {
		return c, missing("username")
	}
	if strings.ContainsAny(c.host, " /\r\n") {
		return c, fmt.Errorf("%w: host must be a bare host name", ErrConfig)
	}
	switch c.security {
	case "":
		c.security = "ssl"
	case "ssl", "starttls", "none":
	default:
		return c, fmt.Errorf("%w: security must be ssl, starttls or none", ErrConfig)
	}
	if c.security == "none" && !imapLoopback(c.host) {
		return c, fmt.Errorf("%w: security none sends the password unencrypted and is "+
			"only allowed for localhost; use ssl or starttls", ErrConfig)
	}
	if c.port == "" {
		c.port = "993"
		if c.security != "ssl" {
			c.port = "143"
		}
	}
	if n, err := strconv.Atoi(c.port); err != nil || n < 1 || n > 65535 {
		return c, fmt.Errorf("%w: port must be a number between 1 and 65535", ErrConfig)
	}
	for _, f := range strings.FieldsFunc(cfg.Get("folders"), func(r rune) bool { return r == ',' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			if strings.ContainsAny(f, "\r\n\x00") {
				return c, fmt.Errorf("%w: folder names cannot contain control characters", ErrConfig)
			}
			c.folders = append(c.folders, f)
		}
	}
	if len(c.folders) == 0 {
		c.folders = []string{"INBOX"}
	}
	if v := cfg.Get("max_threads"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("%w: max_threads must be a positive number", ErrConfig)
		}
		c.maxThreads = n
	}
	if cfg.Get("since") != "" {
		if _, err := timeParseDay(cfg.Get("since")); err != nil {
			return c, fmt.Errorf("%w: since must be YYYY-MM-DD", ErrConfig)
		}
	}
	return c, nil
}

func imapLoopback(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ---- wire protocol ---------------------------------------------------------

// imapPart is one piece of a command: raw text, or a literal.
type imapPart struct {
	s    string
	lit  bool
	data []byte
}

func imapRaw(s string) imapPart { return imapPart{s: s} }

// imapStr is a quoted string when it can be, a literal when it must be.
func imapStr(s string) imapPart {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return imapPart{lit: true, data: []byte(s)}
		}
	}
	return imapPart{s: `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`}
}

func (p imapPart) nonASCII() bool {
	if !p.lit {
		return false
	}
	for _, b := range p.data {
		if b > 0x7e {
			return true
		}
	}
	return false
}

type imapResp struct {
	text string   // the line, with every literal replaced by a NUL marker
	lits [][]byte // the literals, in order; nil when over the size cap
}

type imapConn struct {
	conn   net.Conn
	r      *bufio.Reader
	n      int
	secret string
	stop   func() bool
}

var imapUIDRE = regexp.MustCompile(`(?i)\bUID (\d+)`)
var imapValidityRE = regexp.MustCompile(`(?i)\[UIDVALIDITY (\d+)\]`)
var imapLiteralRE = regexp.MustCompile(`\{(\d+)\}$`)

const imapIOTimeout = 90 * time.Second

// scrub removes the credential from anything that might be shown to a person.
// A server is free to echo what it was sent into an error message.
func (c *imapConn) scrub(s string) string {
	if c.secret != "" {
		s = strings.ReplaceAll(s, c.secret, "***")
	}
	return s
}

func imapScrub(secret string, err error) error {
	if err == nil || secret == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, secret, "***"))
}

func imapDial(ctx context.Context, cfg imapConfig, secret string) (*imapConn, error) {
	addr := net.JoinHostPort(cfg.host, cfg.port)
	d := net.Dialer{Timeout: 30 * time.Second}
	tlsCfg := &tls.Config{ServerName: cfg.host, MinVersion: tls.VersionTLS12}
	var raw net.Conn
	var err error
	if cfg.security == "ssl" {
		raw, err = (&tls.Dialer{NetDialer: &d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		raw, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("imap: connect %s: %w", addr, err)
	}
	c := &imapConn{conn: raw, r: bufio.NewReader(raw), secret: secret}
	c.stop = context.AfterFunc(ctx, func() { raw.Close() })
	fail := func(err error) (*imapConn, error) {
		c.close()
		return nil, imapScrub(secret, err)
	}
	greet, err := c.line()
	if err != nil {
		return fail(fmt.Errorf("imap: reading greeting: %w", err))
	}
	if !strings.HasPrefix(greet, "* OK") && !strings.HasPrefix(greet, "* PREAUTH") {
		return fail(fmt.Errorf("imap: server refused the connection: %s", greet))
	}
	if cfg.security == "starttls" {
		if _, err := c.cmd(imapRaw("STARTTLS")); err != nil {
			return fail(fmt.Errorf("imap: STARTTLS: %w", err))
		}
		tc := tls.Client(raw, tlsCfg)
		_ = raw.SetDeadline(time.Now().Add(imapIOTimeout))
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail(fmt.Errorf("imap: TLS handshake: %w", err))
		}
		c.conn, c.r = tc, bufio.NewReader(tc)
	}
	if strings.ContainsAny(cfg.user+secret, "\r\n") {
		return fail(fmt.Errorf("imap: credentials contain a line break"))
	}
	if _, err := c.cmd(imapRaw("LOGIN "), imapStr(cfg.user), imapRaw(" "), imapStr(secret)); err != nil {
		return fail(fmt.Errorf("imap: login failed: %w", err))
	}
	return c, nil
}

func (c *imapConn) close() {
	if c.stop != nil {
		c.stop()
	}
	_ = c.conn.Close()
}

// logout says goodbye politely, then closes.
func (c *imapConn) logout() {
	_, _ = c.cmd(imapRaw("LOGOUT"))
	c.close()
}

func (c *imapConn) line() (string, error) {
	_ = c.conn.SetDeadline(time.Now().Add(imapIOTimeout))
	s, err := c.r.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// cmd sends one tagged command and collects the untagged responses.
func (c *imapConn) cmd(parts ...imapPart) ([]imapResp, error) {
	c.n++
	tag := fmt.Sprintf("G%04d", c.n)
	var buf bytes.Buffer
	buf.WriteString(tag + " ")
	flush := func() error {
		_ = c.conn.SetDeadline(time.Now().Add(imapIOTimeout))
		_, err := c.conn.Write(buf.Bytes())
		buf.Reset()
		return err
	}
	for _, p := range parts {
		if !p.lit {
			buf.WriteString(p.s)
			continue
		}
		fmt.Fprintf(&buf, "{%d}\r\n", len(p.data))
		if err := flush(); err != nil {
			return nil, err
		}
		cont, err := c.line()
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(cont, "+") {
			return nil, fmt.Errorf("imap: server rejected a literal: %s", cont)
		}
		buf.Write(p.data)
	}
	buf.WriteString("\r\n")
	if err := flush(); err != nil {
		return nil, err
	}

	var out []imapResp
	for {
		l, err := c.line()
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(l, tag+" ") {
			status := strings.TrimSpace(l[len(tag)+1:])
			if len(status) >= 2 && strings.EqualFold(status[:2], "OK") {
				return out, nil
			}
			return nil, errors.New(c.scrub(status))
		}
		resp := imapResp{}
		var text strings.Builder
		for {
			m := imapLiteralRE.FindStringSubmatchIndex(l)
			if m == nil {
				text.WriteString(l)
				break
			}
			n, _ := strconv.Atoi(l[m[2]:m[3]])
			text.WriteString(l[:m[0]])
			text.WriteString("\x00")
			if n > maxBody {
				if _, err := io.CopyN(io.Discard, c.r, int64(n)); err != nil {
					return nil, err
				}
				resp.lits = append(resp.lits, nil)
			} else {
				_ = c.conn.SetDeadline(time.Now().Add(imapIOTimeout))
				b := make([]byte, n)
				if _, err := io.ReadFull(c.r, b); err != nil {
					return nil, err
				}
				resp.lits = append(resp.lits, b)
			}
			if l, err = c.line(); err != nil {
				return nil, err
			}
		}
		resp.text = text.String()
		out = append(out, resp)
	}
}

// examine opens a folder read-only and returns its UIDVALIDITY.
func (c *imapConn) examine(folder string) (uint32, error) {
	resps, err := c.cmd(imapRaw("EXAMINE "), imapStr(folder))
	if err != nil {
		return 0, fmt.Errorf("imap: folder %q: %w", folder, err)
	}
	for _, r := range resps {
		if m := imapValidityRE.FindStringSubmatch(r.text); m != nil {
			n, _ := strconv.ParseUint(m[1], 10, 32)
			return uint32(n), nil
		}
	}
	return 0, nil
}

// search runs UID SEARCH with the given criteria and returns ascending UIDs.
func (c *imapConn) search(criteria ...imapPart) ([]uint32, error) {
	parts := []imapPart{imapRaw("UID SEARCH ")}
	for _, p := range criteria {
		if p.nonASCII() {
			parts = append(parts, imapRaw("CHARSET UTF-8 "))
			break
		}
	}
	resps, err := c.cmd(append(parts, criteria...)...)
	if err != nil {
		return nil, err
	}
	var uids []uint32
	for _, r := range resps {
		f := strings.Fields(r.text)
		if len(f) >= 2 && f[0] == "*" && strings.EqualFold(f[1], "SEARCH") {
			for _, s := range f[2:] {
				if n, err := strconv.ParseUint(s, 10, 32); err == nil {
					uids = append(uids, uint32(n))
				}
			}
		}
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids, nil
}

// fetch returns the full message for each UID (PEEK: the \Seen flag stays put).
func (c *imapConn) fetch(uids []uint32) (map[uint32][]byte, error) {
	out := map[uint32][]byte{}
	for i := 0; i < len(uids); i += 25 {
		end := i + 25
		if end > len(uids) {
			end = len(uids)
		}
		ids := make([]string, 0, end-i)
		for _, u := range uids[i:end] {
			ids = append(ids, strconv.FormatUint(uint64(u), 10))
		}
		resps, err := c.cmd(imapRaw("UID FETCH " + strings.Join(ids, ",") + " (BODY.PEEK[])"))
		if err != nil {
			return nil, err
		}
		for _, r := range resps {
			m := imapUIDRE.FindStringSubmatch(r.text)
			if m == nil || len(r.lits) == 0 || r.lits[0] == nil {
				continue
			}
			n, _ := strconv.ParseUint(m[1], 10, 32)
			out[uint32(n)] = r.lits[0]
		}
	}
	return out, nil
}

// ---- message parsing -------------------------------------------------------

type imapMsg struct {
	MailMessage
	folder string
	uid    uint32
	msgID  string
	root   string
	ids    map[string]bool // every id this message names: its own, References, In-Reply-To
}

var imapIDRE = regexp.MustCompile(`<[^<>\s]+>`)

func imapDecoder() *mime.WordDecoder {
	return &mime.WordDecoder{CharsetReader: func(charset string, in io.Reader) (io.Reader, error) {
		return imapCharsetReader(charset, in), nil
	}}
}

// imapCharsetReader decodes to UTF-8, leniently: an unknown charset passes
// through as-is rather than failing the whole message.
func imapCharsetReader(charset string, in io.Reader) io.Reader {
	cs := strings.ToLower(strings.TrimSpace(charset))
	if cs == "" || cs == "utf-8" || cs == "utf8" || cs == "us-ascii" {
		return in
	}
	enc, err := htmlindex.Get(cs)
	if err != nil {
		return in
	}
	return enc.NewDecoder().Reader(in)
}

func imapHeader(h mail.Header, key string) string {
	v := h.Get(key)
	if d, err := imapDecoder().DecodeHeader(v); err == nil {
		v = d
	}
	return strings.TrimSpace(v)
}

func imapAddrs(h mail.Header, key string) (display []string, bare []string) {
	raw := h.Get(key)
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	p := mail.AddressParser{WordDecoder: imapDecoder()}
	list, err := p.ParseList(raw)
	if err != nil {
		return []string{imapHeader(h, key)}, nil
	}
	for _, a := range list {
		display = append(display, a.String())
		bare = append(bare, strings.ToLower(a.Address))
	}
	return display, bare
}

var imapSentFolders = map[string]bool{"sent": true, "sent items": true, "sent mail": true, "sent messages": true}

func imapIsSentFolder(folder string) bool {
	f := strings.ToLower(folder)
	if i := strings.LastIndexAny(f, "/."); i >= 0 {
		f = f[i+1:]
	}
	return imapSentFolders[strings.TrimSpace(f)]
}

func parseIMAPMessage(raw []byte, folder string, uid uint32, user string) (*imapMsg, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	m := &imapMsg{folder: folder, uid: uid, ids: map[string]bool{}}
	h := msg.Header
	m.Subject = imapHeader(h, "Subject")
	from, fromBare := imapAddrs(h, "From")
	if len(from) > 0 {
		m.From = from[0]
	}
	if len(fromBare) > 0 {
		m.FromAddr = fromBare[0]
	}
	m.To, _ = imapAddrs(h, "To")
	m.Cc, _ = imapAddrs(h, "Cc")
	if t, err := h.Date(); err == nil {
		m.Date = rfc3339(t)
	}
	m.msgID = imapIDRE.FindString(h.Get("Message-Id"))
	refs := imapIDRE.FindAllString(h.Get("References"), -1)
	irt := imapIDRE.FindAllString(h.Get("In-Reply-To"), -1)
	switch {
	case len(refs) > 0:
		m.root = refs[0]
	case len(irt) > 0:
		m.root = irt[0]
	case m.msgID != "":
		m.root = m.msgID
	default:
		m.root = "subject:" + strings.ToLower(NormalizeSubject(m.Subject))
	}
	for _, id := range append(append(refs, irt...), m.msgID) {
		if id != "" {
			m.ids[id] = true
		}
	}
	m.ID = firstNonEmpty(m.msgID, fmt.Sprintf("%s|%d", folder, uid))
	user = strings.ToLower(strings.TrimSpace(user))
	m.Sent = imapIsSentFolder(folder) || (strings.Contains(user, "@") && m.FromAddr == user)

	var w imapWalk
	w.entity(textprotoHeader(h), msg.Body, 0)
	m.Body = strings.TrimSpace(strings.Join(w.plain, "\n\n"))
	if m.Body == "" {
		m.Body = strings.TrimSpace(strings.Join(w.html, "\n\n"))
	}
	m.Attachments = w.attachments
	return m, nil
}

// textprotoHeader adapts mail.Header (a map of string slices) to the
// MIME-header shape the walker uses for parts.
func textprotoHeader(h mail.Header) map[string][]string { return h }

type imapWalk struct {
	plain, html, attachments []string
}

func imapGet(h map[string][]string, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

const imapMaxText = 1 << 20

// entity walks one MIME entity. depth bounds pathological nesting.
func (w *imapWalk) entity(h map[string][]string, body io.Reader, depth int) {
	ct, params, err := mime.ParseMediaType(imapGet(h, "Content-Type"))
	if err != nil || ct == "" {
		ct, params = "text/plain", map[string]string{}
	}
	ct = strings.ToLower(ct)
	disp, dparams, _ := mime.ParseMediaType(imapGet(h, "Content-Disposition"))
	name := firstNonEmpty(dparams["filename"], params["name"])
	if d, err := imapDecoder().DecodeHeader(name); err == nil {
		name = d
	}
	attachment := strings.EqualFold(disp, "attachment") || (name != "" && !strings.HasPrefix(ct, "multipart/"))

	if strings.HasPrefix(ct, "multipart/") && !attachment && depth < 8 {
		boundary := params["boundary"]
		if boundary == "" {
			return
		}
		mr := multipart.NewReader(body, boundary)
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				return
			}
			w.entity(p.Header, p, depth+1)
		}
	}

	decoded := imapDecodeBody(imapGet(h, "Content-Transfer-Encoding"), body)
	if attachment || (!strings.HasPrefix(ct, "text/") && !strings.HasPrefix(ct, "multipart/")) {
		// Count the bytes, keep none of them.
		n, _ := io.Copy(io.Discard, decoded)
		if name == "" {
			name = "unnamed"
		}
		w.attachments = append(w.attachments, fmt.Sprintf("%s (%s, %d bytes)", name, ct, n))
		return
	}
	if !strings.HasPrefix(ct, "text/") {
		return
	}
	b, _ := io.ReadAll(io.LimitReader(imapCharsetReader(params["charset"], decoded), imapMaxText))
	text := strings.ToValidUTF8(string(b), "�")
	switch ct {
	case "text/plain":
		if t := strings.TrimSpace(text); t != "" {
			w.plain = append(w.plain, t)
		}
	case "text/html":
		if t := mailText("text/html", text); t != "" {
			w.html = append(w.html, t)
		}
	}
}

func imapDecodeBody(cte string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &imapB64Clean{r})
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// imapB64Clean drops everything that is not base64, so stray whitespace or a
// trailing line from a sloppy sender does not abort the decode.
type imapB64Clean struct{ r io.Reader }

func (c *imapB64Clean) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	j := 0
	for _, b := range p[:n] {
		if b == '=' || b == '+' || b == '/' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
			p[j] = b
			j++
		}
	}
	if j == 0 && err == nil {
		return c.Read(p)
	}
	return j, err
}

// ---- threads ---------------------------------------------------------------

type imapThread struct {
	root string
	msgs []*imapMsg
	have map[string]bool // folder|uid
	seen map[string]bool // message-id, so one message in two folders counts once
}

func newIMAPThread(root string) *imapThread {
	return &imapThread{root: root, have: map[string]bool{}, seen: map[string]bool{}}
}

func (t *imapThread) add(m *imapMsg) {
	key := fmt.Sprintf("%s|%d", m.folder, m.uid)
	if t.have[key] {
		return
	}
	t.have[key] = true
	if m.msgID != "" {
		if t.seen[m.msgID] {
			return
		}
		t.seen[m.msgID] = true
	}
	t.msgs = append(t.msgs, m)
}

func (t *imapThread) document(host string) Document {
	mm := make([]MailMessage, 0, len(t.msgs))
	var folders []string
	seen := map[string]bool{}
	for _, m := range t.msgs {
		mm = append(mm, m.MailMessage)
		if !seen[m.folder] {
			seen[m.folder] = true
			folders = append(folders, m.folder)
		}
	}
	doc := ThreadDocument(mm)
	sum := sha1.Sum([]byte(strings.ToLower(host) + "\x00" + t.root))
	doc.ExternalID = "imap:" + hex.EncodeToString(sum[:8])
	doc.Meta["source"] = "imap"
	doc.Meta["folder"] = strings.Join(folders, ", ")
	return doc
}

func (t *imapThread) attachments() []string {
	var out []string
	for _, m := range t.msgs {
		out = append(out, m.Attachments...)
	}
	return out
}

// complete pulls the rest of each thread from every folder given.
func imapComplete(c *imapConn, user string, folders []string, threads []*imapThread) error {
	for _, folder := range folders {
		if _, err := c.examine(folder); err != nil {
			return err
		}
		for _, t := range threads {
			var crit []imapPart
			if strings.HasPrefix(t.root, "subject:") {
				crit = []imapPart{imapRaw("SUBJECT "), imapStr(strings.TrimPrefix(t.root, "subject:"))}
			} else {
				id := imapStr(t.root)
				crit = []imapPart{imapRaw("OR HEADER References "), id,
					imapRaw(" OR HEADER Message-ID "), id,
					imapRaw(" HEADER In-Reply-To "), id}
			}
			uids, err := c.search(crit...)
			if err != nil {
				return err
			}
			var want []uint32
			for _, u := range uids {
				if !t.have[fmt.Sprintf("%s|%d", folder, u)] {
					want = append(want, u)
				}
			}
			raws, err := c.fetch(want)
			if err != nil {
				return err
			}
			for _, u := range want {
				raw, ok := raws[u]
				if !ok {
					continue
				}
				m, err := parseIMAPMessage(raw, folder, u, user)
				if err != nil {
					continue
				}
				if m.ids[t.root] || m.root == t.root {
					t.add(m)
				}
			}
		}
	}
	return nil
}

// ---- cursor ----------------------------------------------------------------

type imapFolderState struct{ validity, last uint32 }

func imapDecodeCursor(s string) map[string]imapFolderState {
	out := map[string]imapFolderState{}
	for _, ent := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(ent, "=")
		if !ok {
			continue
		}
		name, err := url.QueryUnescape(k)
		a, b, ok := strings.Cut(v, ":")
		if err != nil || !ok {
			continue
		}
		val, e1 := strconv.ParseUint(a, 10, 32)
		last, e2 := strconv.ParseUint(b, 10, 32)
		if e1 != nil || e2 != nil {
			continue
		}
		out[name] = imapFolderState{uint32(val), uint32(last)}
	}
	return out
}

func imapEncodeCursor(folders []string, st map[string]imapFolderState) string {
	var parts []string
	for _, f := range folders {
		if s, ok := st[f]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d:%d", url.QueryEscape(f), s.validity, s.last))
		}
	}
	return strings.Join(parts, ";")
}

// ---- Source ----------------------------------------------------------------

func imapSinceCriterion(cfg Config) (imapPart, error) {
	s, err := sinceCutoff(cfg, 30)
	if err != nil {
		return imapPart{}, err
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return imapPart{}, err
	}
	return imapRaw("SINCE " + t.UTC().Format("02-Jan-2006")), nil
}

func (imapSource) Fetch(ctx context.Context, in Input) (page Page, err error) {
	cfg, err := parseIMAPConfig(in.Config)
	if err != nil {
		return Page{}, err
	}
	if in.Secret == "" {
		return Page{}, missing("an app password")
	}
	defer func() { err = imapScrub(in.Secret, err) }()
	since, err := imapSinceCriterion(in.Config)
	if err != nil {
		return Page{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if cfg.maxThreads > 0 && cfg.maxThreads < limit {
		limit = cfg.maxThreads
	}

	c, err := imapDial(ctx, cfg, in.Secret)
	if err != nil {
		return Page{}, err
	}
	defer c.logout()

	state := imapDecodeCursor(in.Cursor)
	var order []*imapThread
	byRoot := map[string]*imapThread{}
	more := false

folders:
	for _, folder := range cfg.folders {
		validity, err := c.examine(folder)
		if err != nil {
			return Page{}, err
		}
		cur, known := state[folder]
		if !known || cur.validity != validity || cur.last == 0 {
			// First sync, or the server renumbered the folder: UIDs mean
			// something else now, so start over from the cutoff.
			cur = imapFolderState{validity: validity}
		}
		var uids []uint32
		if cur.last == 0 {
			uids, err = c.search(since)
		} else {
			uids, err = c.search(imapRaw(fmt.Sprintf("UID %d:*", cur.last+1)))
		}
		if err != nil {
			return Page{}, err
		}
		var fresh []uint32
		for _, u := range uids {
			if u > cur.last {
				fresh = append(fresh, u)
			}
		}
		for i := 0; i < len(fresh); i += 25 {
			end := i + 25
			if end > len(fresh) {
				end = len(fresh)
			}
			raws, err := c.fetch(fresh[i:end])
			if err != nil {
				return Page{}, err
			}
			for _, u := range fresh[i:end] {
				raw, ok := raws[u]
				var m *imapMsg
				if ok {
					m, err = parseIMAPMessage(raw, folder, u, cfg.user)
				}
				if !ok || err != nil {
					// Expunged meanwhile, or not parseable: nothing to
					// return, so step past it rather than retrying forever.
					cur.last = u
					continue
				}
				t := byRoot[m.root]
				if t == nil {
					if len(order) >= limit {
						more = true
						state[folder] = cur
						break folders
					}
					t = newIMAPThread(m.root)
					byRoot[m.root] = t
					order = append(order, t)
				}
				t.add(m)
				cur.last = u
			}
		}
		state[folder] = cur
	}

	if len(order) > 0 {
		if err := imapComplete(c, cfg.user, imapAllFolders(cfg.folders, nil), order); err != nil {
			return Page{}, err
		}
	}
	docs := make([]Document, 0, len(order))
	for _, t := range order {
		docs = append(docs, t.document(cfg.host))
	}
	return Page{Docs: docs, Cursor: imapEncodeCursor(cfg.folders, state), More: more}, nil
}

func imapAllFolders(folders []string, extra []string) []string {
	out := append([]string{}, folders...)
	have := map[string]bool{}
	for _, f := range out {
		have[f] = true
	}
	for _, f := range extra {
		if !have[f] {
			out = append(out, f)
		}
	}
	return out
}

// ---- Searcher / Reader -----------------------------------------------------

func (imapSource) Search(ctx context.Context, in Input, query string, limit int) (hits []Hit, err error) {
	cfg, err := parseIMAPConfig(in.Config)
	if err != nil {
		return nil, err
	}
	if in.Secret == "" {
		return nil, missing("an app password")
	}
	if strings.ContainsAny(query, "\r\n") {
		return nil, fmt.Errorf("%w: search text cannot contain line breaks", ErrConfig)
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: search text is empty", ErrConfig)
	}
	defer func() { err = imapScrub(in.Secret, err) }()
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	c, err := imapDial(ctx, cfg, in.Secret)
	if err != nil {
		return nil, err
	}
	defer c.logout()

	type dated struct {
		hit  Hit
		date string
		uid  uint32
	}
	var all []dated
	for _, folder := range cfg.folders {
		if _, err := c.examine(folder); err != nil {
			return nil, err
		}
		uids, err := c.search(imapRaw("TEXT "), imapStr(query))
		if err != nil {
			return nil, err
		}
		if len(uids) > limit {
			uids = uids[len(uids)-limit:] // highest UIDs are the newest
		}
		raws, err := c.fetch(uids)
		if err != nil {
			return nil, err
		}
		for _, u := range uids {
			raw, ok := raws[u]
			if !ok {
				continue
			}
			m, err := parseIMAPMessage(raw, folder, u, cfg.user)
			if err != nil {
				continue
			}
			all = append(all, dated{Hit{
				ID:      fmt.Sprintf("%s|%d", folder, u),
				Title:   firstNonEmpty(m.Subject, "(no subject)"),
				Snippet: imapSnippet(m.Body),
				Updated: m.Date,
				Author:  m.From,
			}, m.Date, u})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].date != all[j].date {
			return all[i].date > all[j].date
		}
		return all[i].uid > all[j].uid
	})
	if len(all) > limit {
		all = all[:limit]
	}
	for _, d := range all {
		hits = append(hits, d.hit)
	}
	return hits, nil
}

func imapSnippet(body string) string {
	s := strings.Join(strings.Fields(body), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

func (imapSource) Read(ctx context.Context, in Input, id string) (item Item, err error) {
	cfg, err := parseIMAPConfig(in.Config)
	if err != nil {
		return Item{}, err
	}
	if in.Secret == "" {
		return Item{}, missing("an app password")
	}
	defer func() { err = imapScrub(in.Secret, err) }()
	i := strings.LastIndex(id, "|")
	if i <= 0 {
		return Item{}, fmt.Errorf("imap: item id %q is not folder|uid", id)
	}
	folder := id[:i]
	uid64, perr := strconv.ParseUint(id[i+1:], 10, 32)
	if perr != nil {
		return Item{}, fmt.Errorf("imap: item id %q is not folder|uid", id)
	}
	c, err := imapDial(ctx, cfg, in.Secret)
	if err != nil {
		return Item{}, err
	}
	defer c.logout()
	if _, err := c.examine(folder); err != nil {
		return Item{}, err
	}
	raws, err := c.fetch([]uint32{uint32(uid64)})
	if err != nil {
		return Item{}, err
	}
	raw, ok := raws[uint32(uid64)]
	if !ok {
		return Item{}, fmt.Errorf("imap: message %s not found", id)
	}
	m, err := parseIMAPMessage(raw, folder, uint32(uid64), cfg.user)
	if err != nil {
		return Item{}, fmt.Errorf("imap: message %s is not readable: %w", id, err)
	}
	t := newIMAPThread(m.root)
	t.add(m)
	if err := imapComplete(c, cfg.user, imapAllFolders(cfg.folders, []string{folder}), []*imapThread{t}); err != nil {
		return Item{}, err
	}
	doc := t.document(cfg.host)
	return Item{
		ID:          id,
		Title:       doc.Title,
		Body:        doc.Body,
		Updated:     doc.Updated,
		Author:      doc.Author,
		Attachments: t.attachments(),
	}, nil
}
