package agenthook

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedHookMatchesTheClientCopy(t *testing.T) {
	want, err := os.ReadFile("../../../clients/hooks/" + FileName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Script, want) {
		t.Fatal("go/internal/agenthook/" + FileName + " differs from clients/hooks/" + FileName +
			"; copy the client file over it")
	}
}
