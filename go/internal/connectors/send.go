package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// postJSON sends a JSON body with the bearer credential and decodes the reply.
// Shared by every action: one place to bound the response and report a
// failure legibly.
func postJSON(ctx context.Context, in Input, rawURL string, body any, out any) error {
	return sendJSON(ctx, in, http.MethodPost, rawURL, body, out, map[string]string{
		"Authorization": "Bearer " + in.Secret})
}

func sendJSON(ctx context.Context, in Input, method, rawURL string, body any, out any, headers map[string]string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grimoire-connector")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := in.Client
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, req.URL.Host, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return statusError(req, resp.StatusCode, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: response was not JSON: %w", req.URL.Host, err)
	}
	return nil
}

func mimeHeader(s string) string {
	if s == "" {
		return ""
	}
	return strings.TrimSpace(mime.QEncoding.Encode("UTF-8", s))
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func http_LimitReader(resp *http.Response) io.Reader { return io.LimitReader(resp.Body, 1<<20) }
