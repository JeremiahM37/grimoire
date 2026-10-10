package bank

import (
	"context"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// PieceRequest is one piece of text to extract facts from without a bank: the
// same extractor Retain uses, for callers (such as the grounded-answer
// timeline) that want the facts but keep their own storage.
type PieceRequest struct {
	Text      string
	EventDate time.Time // zero when the piece has no time
	Context   string    // who/what the piece is, in a line
	Mission   string    // what the facts are for; optional
	Index     int
	Total     int
}

// PieceFact is one extracted fact.
type PieceFact struct {
	Text     string
	Event    bool      // an event (happened at a time) rather than a state
	Start    time.Time // occurrence range of an event; zero for states
	End      time.Time
	Entities []string
}

// ExtractPiece runs the bank's extractor over one piece. It performs no
// storage, entity resolution or deduplication.
func ExtractPiece(ctx context.Context, client *ai.Client, req PieceRequest) ([]PieceFact, usage.Tokens, error) {
	e := &Engine{}
	got, spent, err := e.extractChunk(ctx, client, extractInput{
		Chunk: req.Text, Index: req.Index, Total: req.Total, EventDate: req.EventDate,
		Context: req.Context, Mission: req.Mission, Mode: ModeConcise,
	})
	if err != nil {
		return nil, spent, err
	}
	out := make([]PieceFact, 0, len(got))
	for _, f := range got {
		out = append(out, PieceFact{Text: f.Text, Event: f.Kind == "event", Start: f.OccStart, End: f.OccEnd, Entities: f.Entities})
	}
	return out, spent, nil
}
