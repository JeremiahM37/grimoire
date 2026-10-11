package cascade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/decide"
)

// fake serves one decision answer with the given replace probability.
func fake(t *testing.T, p float64, status int) *decide.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "down", status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": map[string]any{
			"replace": map[string]any{"type": "choice", "choice": "replace",
				"probabilities": map[string]float64{"replace": p, "distinct": 1 - p}}}})
	}))
	t.Cleanup(srv.Close)
	return &decide.Client{BaseURL: srv.URL, Model: "fake", HTTP: &http.Client{Timeout: 2 * time.Second}}
}

const old, new = "the billing postgres replica lags 2 seconds", "the analytics postgres replica lags 40 seconds"

func TestNoServerKeepsBoth(t *testing.T) {
	v := Gate{Lo: 0.2, Hi: 0.8, LayaCal: Identity, JevCal: Identity}.Decide(context.Background(), old, new)
	if v.Replace || v.Source != "fallback" {
		t.Fatalf("no server: got %+v, want keep via fallback", v)
	}
}

func TestLayaDecidesOutsideTheBand(t *testing.T) {
	g := Gate{Laya: ClientAsker{fake(t, 0.95, 200)}, LayaCal: Identity, Jev: ClientAsker{fake(t, 0, 200)},
		JevCal: Identity, Lo: 0.2, Hi: 0.8}
	if v := g.Decide(context.Background(), old, new); !v.Replace || v.Source != "laya" {
		t.Fatalf("confident laya replace: got %+v", v)
	}
	g.Laya = ClientAsker{fake(t, 0.05, 200)}
	if v := g.Decide(context.Background(), old, new); v.Replace || v.Source != "laya" {
		t.Fatalf("confident laya keep: got %+v", v)
	}
}

func TestBandEscalatesToJev(t *testing.T) {
	g := Gate{Laya: ClientAsker{fake(t, 0.5, 200)}, LayaCal: Identity, Jev: ClientAsker{fake(t, 0.9, 200)},
		JevCal: Identity, Lo: 0.2, Hi: 0.8}
	if v := g.Decide(context.Background(), old, new); !v.Replace || v.Source != "jev" {
		t.Fatalf("in-band pair should go to jev: got %+v", v)
	}
}

func TestFailuresKeepBothNeverSupersede(t *testing.T) {
	// Laya errors and no Jev: keep both.
	g := Gate{Laya: ClientAsker{fake(t, 0.99, 500)}, LayaCal: Identity, Lo: 0.2, Hi: 0.8}
	if v := g.Decide(context.Background(), old, new); v.Replace || v.Source != "fallback" {
		t.Fatalf("laya error: got %+v, want fallback keep", v)
	}
	// Laya in band and Jev errors: keep both.
	g = Gate{Laya: ClientAsker{fake(t, 0.5, 200)}, LayaCal: Identity,
		Jev: ClientAsker{fake(t, 0.99, 503)}, JevCal: Identity, Lo: 0.2, Hi: 0.8}
	if v := g.Decide(context.Background(), old, new); v.Replace || v.Source != "fallback" {
		t.Fatalf("jev error: got %+v, want fallback keep", v)
	}
}

func TestPlattAndBandParsing(t *testing.T) {
	p, err := ParsePlatt("1.5, -0.25")
	if err != nil || p != (Platt{A: 1.5, B: -0.25}) {
		t.Fatalf("ParsePlatt = %+v, %v", p, err)
	}
	if _, err := ParsePlatt("x,1"); err == nil {
		t.Error("ParsePlatt accepted a non-number")
	}
	if got := Identity.Apply(0.7); got < 0.6999 || got > 0.7001 {
		t.Errorf("identity calibration moved 0.7 to %v", got)
	}
	if lo, hi, err := ParseBand("0.05,0.65"); err != nil || lo != 0.05 || hi != 0.65 {
		t.Fatalf("ParseBand = %v %v %v", lo, hi, err)
	}
	if _, _, err := ParseBand("0.9,0.1"); err == nil {
		t.Error("ParseBand accepted lo > hi")
	}
}
