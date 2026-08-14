package embed

import (
	"reflect"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/compat"
)

type chunkFixture struct {
	ChunkCases []struct {
		Text   string   `json:"text"`
		Chunks []string `json:"chunks"`
	} `json:"chunk_cases"`
}

// Chunk boundaries decide what retrieval can return at all, so a divergence
// here changes results without any error surfacing.
func TestChunkTextMatchesPython(t *testing.T) {
	var fx chunkFixture
	compat.Load(t, "embed.json", &fx)
	if len(fx.ChunkCases) == 0 {
		t.Fatal("no chunk cases in fixture")
	}
	for i, c := range fx.ChunkCases {
		got := ChunkText(c.Text)
		want := c.Chunks
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("case %d (%d chars): got %d chunks, want %d\n got: %q\nwant: %q",
				i, len(c.Text), len(got), len(want), got, want)
		}
	}
}
