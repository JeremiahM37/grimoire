package api

import (
	"bytes"
	"net/http"
)

// captureWriter collects a handler's response instead of sending it.
//
// A batch endpoint runs the single-item handler once per item, and each of
// those handlers writes a status and a body. Without this they would write
// them to the real connection — the first item would send the whole response
// and the rest would log "superfluous WriteHeader". Capturing lets the batch
// keep one handler as the single implementation of what a write means,
// including every validation and authorization check in it, rather than
// growing a second copy that drifts.
type captureWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (c *captureWriter) Header() http.Header { return http.Header{} }

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(p)
}

func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
