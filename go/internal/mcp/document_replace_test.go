package mcp

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDocumentReplacementSendsExplicitGeneratedPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("path") != "documents/original.md" {
			t.Errorf("replacement path = %q", r.FormValue("path"))
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		body, _ := io.ReadAll(file)
		if string(body) != "replacement text" || header.Filename != "replacement.txt" {
			t.Errorf("unexpected file %s: %q", header.Filename, body)
		}
		_, _ = io.WriteString(w, `{"path":"documents/original.md"}`)
	}))
	defer backend.Close()
	server := &Server{BaseURL: backend.URL, Client: backend.Client()}
	_, err := server.dispatch("import_document", map[string]any{"filename": "replacement.txt", "content": base64.StdEncoding.EncodeToString([]byte("replacement text")), "path": "documents/original.md"})
	if err != nil {
		t.Fatal(err)
	}
}
