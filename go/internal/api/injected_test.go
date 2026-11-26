package api

import (
	"strings"
	"testing"
)

func TestInjectedContextLeavesOutCommandsAndVaultValues(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t)
	if err := server.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	value := "plain-router-" + "password-77"
	if err := server.Secrets.Put("ROUTER_PASSWORD", value, nil); err != nil {
		t.Fatal(err)
	}
	remember(t, handler, map[string]any{"topic": "kestrel",
		"text": "The kestrel router admin login is " + value + " for the web console"})
	remember(t, handler, map[string]any{"topic": "kestrel",
		"text": "Rebuild the kestrel image with `docker build -t kestrel:dev ./images/kestrel` before deploying"})
	response := do(t, handler, "GET", "/api/memory/context?q=kestrel+router+admin+image+deploying&max_bytes=4000", nil)
	var result map[string]any
	decode(t, response, &result)
	context := result["context"].(string)
	if strings.Contains(context, value) || !strings.Contains(context, "[vault secret ROUTER_PASSWORD]") {
		t.Fatalf("vault value not redacted: %s", context)
	}
	if strings.Contains(context, "docker build") || !strings.Contains(context, "(commands in the note)") {
		t.Fatalf("command not condensed: %s", context)
	}
	if !strings.Contains(context, "Rebuild the kestrel image") {
		t.Fatalf("prose lost: %s", context)
	}
}
