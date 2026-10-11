package memimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// Automatic captioning is off unless the operator configures a captioner. When
// one is configured, a picture stored without a human caption is sent to it
// once, and its reply becomes the caption with basis "inferred". A caption the
// caller wrote is never sent anywhere and is never replaced.
//
// The picture leaves the box only for that one request, as the stored bytes
// (EXIF already removed). Nothing is sent when no captioner is configured.

// Settings keys read by CaptionerFrom. They are operator settings, and the
// API key is taken from the environment only, so it is not in settings.json.
const (
	KeyCaptionBackend      = "memory_caption_backend" // "ollama" (default) or "openai"
	KeyCaptionURL          = "memory_caption_url"     // ollama base URL, or an OpenAI-compatible base ending in /v1
	KeyCaptionModel        = "memory_caption_model"   // a vision-capable model
	KeyCaptionAllowPrivate = "memory_caption_allow_private"
	EnvCaptionAPIKey       = "GRIMOIRE_MEMORY_CAPTION_API_KEY"
)

// CaptionTimeout bounds one captioning request, including the model's time.
const CaptionTimeout = 60 * time.Second

// maxCaptionReply bounds what is read back from the captioner.
const maxCaptionReply = 64 << 10

// captionPrompt asks for a description that is useful as a search key. It
// asks for what is visible, not who or where, because a model guessing at
// identity or location would write those into memory as if they were facts.
const captionPrompt = "Describe this picture in one short factual sentence that would help someone find it later. " +
	"Name the main visible objects, text and setting. Do not guess who is shown or where it was taken."

// Getter is the settings view the captioner needs. *settings.Store satisfies it.
type Getter interface {
	Get(key string) string
}

// Captioner writes a caption from the bytes of a picture.
type Captioner interface {
	Caption(ctx context.Context, data []byte, f Format) (string, error)
}

// CaptionerFrom returns the configured captioner, or nil when none is set. A
// nil result is the normal, off-by-default state.
func CaptionerFrom(g Getter) Captioner {
	if g == nil {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(g.Get(KeyCaptionURL)), "/")
	model := strings.TrimSpace(g.Get(KeyCaptionModel))
	if base == "" || model == "" || ValidCaptionURL(base) != nil {
		return nil
	}
	backend := strings.ToLower(strings.TrimSpace(g.Get(KeyCaptionBackend)))
	if backend == "" {
		backend = "ollama"
	}
	if backend != "ollama" && backend != "openai" {
		return nil
	}
	allow := false
	switch strings.ToLower(strings.TrimSpace(g.Get(KeyCaptionAllowPrivate))) {
	case "1", "true", "yes", "on":
		allow = true
	}
	return &httpCaptioner{
		backend: backend, base: base, model: model,
		apiKey: os.Getenv(EnvCaptionAPIKey),
		client: guardedClient(allow, CaptionTimeout),
	}
}

type httpCaptioner struct {
	backend string
	base    string
	model   string
	apiKey  string
	client  *http.Client
}

// Caption sends the picture with a fixed prompt and returns the reply, cleaned
// to one line. Any failure is an error; the caller stores the picture
// uncaptioned rather than failing the upload.
func (c *httpCaptioner) Caption(ctx context.Context, data []byte, f Format) (string, error) {
	if c == nil {
		return "", errors.New("no captioner configured")
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	var (
		endpoint string
		body     any
	)
	if c.backend == "openai" {
		endpoint = c.base + "/chat/completions"
		body = map[string]any{
			"model":       c.model,
			"temperature": 0,
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": captionPrompt},
					map[string]any{"type": "image_url", "image_url": map[string]string{
						"url": "data:" + f.MIME + ";base64," + b64}},
				},
			}},
		}
	} else {
		endpoint = c.base + "/api/chat"
		body = map[string]any{
			"model":   c.model,
			"stream":  false,
			"options": map[string]any{"temperature": 0},
			"messages": []any{map[string]any{
				"role": "user", "content": captionPrompt, "images": []string{b64},
			}},
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("captioner: %w", err)
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxCaptionReply))
	if err != nil {
		return "", fmt.Errorf("captioner: read reply: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("captioner: %s", resp.Status)
	}
	text, err := parseCaptionReply(c.backend, reply)
	if err != nil {
		return "", err
	}
	return CleanCaption(text)
}

// parseCaptionReply extracts the text from either API's reply shape.
func parseCaptionReply(backend string, reply []byte) (string, error) {
	if backend == "openai" {
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(reply, &out); err != nil {
			return "", fmt.Errorf("captioner: bad reply: %w", err)
		}
		if len(out.Choices) == 0 {
			return "", errors.New("captioner: reply has no choices")
		}
		return out.Choices[0].Message.Content, nil
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(reply, &out); err != nil {
		return "", fmt.Errorf("captioner: bad reply: %w", err)
	}
	return out.Message.Content, nil
}

// MaxCaptionChars is the longest caption CleanCaption returns, in runes. It
// matches the limit on human captions.
const MaxCaptionChars = 500

// CleanCaption turns a model reply into one line of plain text. A reply that is
// empty after cleaning is an error, because an empty caption would store a
// picture that looks captioned and is not.
func CleanCaption(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " \"'`")
	if s == "" {
		return "", errors.New("captioner returned no text")
	}
	if r := []rune(s); len(r) > MaxCaptionChars {
		s = string(r[:MaxCaptionChars])
	}
	return s, nil
}

// guardedClient refuses to connect to link-local and cloud-metadata addresses
// always, and to loopback and private networks unless allowPrivate. The check
// runs on every address the name resolves to, at dial time, so a DNS name
// cannot be used to reach an address the URL check would have refused. Proxies
// are not used (they would move the connection past this check), and redirects
// are not followed.
func guardedClient(allowPrivate bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		Proxy:               nil,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if err := secrets.CheckIP(ip.IP, allowPrivate); err != nil {
					// The guard's own message names the broker's variable, which
					// is the wrong knob here, so this error says what applies.
					return nil, fmt.Errorf("captioner address %s refused: %s is needed for private networks; "+
						"link-local and cloud-metadata addresses are never allowed", ip.IP, KeyCaptionAllowPrivate)
				}
			}
			var lastErr error = errors.New("no address for " + host)
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidCaptionURL reports why a configured captioner URL is unusable, without
// connecting. It is a fast check for the settings API; the dial-time guard is
// what actually enforces the address rules.
func ValidCaptionURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("captioner url must be an absolute http or https URL")
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return errors.New("captioner url must be http or https")
	}
	if u.User != nil {
		return errors.New("captioner url must not carry credentials")
	}
	return nil
}
