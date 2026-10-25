package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Settling challenges without asking.
//
// A challenge is an agent's fact that wanted to replace a higher-authority one
// and was refused. Left alone it asks the owner forever, and in practice the
// open ones were not disagreements at all: an event logged against a standing
// preference, or two facts about different things that reconciliation paired
// on shared words. Asking a person to rule on those teaches them to ignore the
// question. So a dream settles every challenge whose answer follows from the
// store itself, and asks only when the evidence really points both ways.
//
// Every ruling is a supersession or a cleared flag, written like any other
// edit with the old text in history, so each one can be reversed.

// challengeVerdict is what the dream decided about one challenge.
type challengeVerdict struct {
	Note, ID, Contested string
	// Action is "clear" (not a real conflict), "uphold" (the contested fact
	// stands), "concede" (the challenger replaces it) or "ask".
	Action string
	Why    string
}

// challengeGrace is how long an agent's challenge to a person's fact stays
// open before the person's fact is upheld by default. A person who wrote the
// fact can still concede later; this only stops it asking.
const challengeGrace = 7 * 24 * time.Hour

// unrelatedBelow is the word overlap under which two facts are not about the
// same thing, whatever reconciliation thought when it paired them.
const unrelatedBelow = 0.2

// anchorState runs a fact's own local checks: "pass" when every one that ran
// passed, "fail" when any failed, "" when it has none that could run.
func anchorState(ctx context.Context, encoded string) string {
	as, err := anchor.Decode(encoded)
	if err != nil || len(as) == 0 {
		return ""
	}
	state := ""
	for _, a := range as {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		res := anchor.Run(c, a)
		cancel()
		switch res.Status {
		case anchor.Fail:
			return "fail"
		case anchor.Pass:
			state = "pass"
		}
	}
	return state
}

// decideChallenge is the ruling, given both facts and the state of their
// checks. Pure, so every rule is tested directly.
func decideChallenge(ch, target memory.Entry, targetLive bool, chState, targetState string, now time.Time) (string, string) {
	switch {
	case !targetLive:
		return "clear", "the fact it contested is gone"
	case ch.Event || target.Event:
		return "clear", "an event records what happened and cannot contradict a standing fact"
	case memory.Similarity(ch.Text, target.Text) < unrelatedBelow:
		return "clear", "the two facts are about different things"
	case targetState == "fail" && chState == "pass":
		return "concede", "the contested fact's own check fails and the challenger's passes"
	case targetState == "fail":
		return "ask", "the contested fact's check fails, but nothing confirms the challenger"
	case targetState == "pass" && chState == "pass":
		// Each claim's own probe confirms it right now, so both are true and
		// they do not contradict each other; reconciliation paired them on
		// shared words (a deploy record against "it is deployed").
		return "clear", "both facts' checks pass, so both are true"
	case targetState == "pass":
		return "uphold", "the contested fact's check passes"
	case chState == "fail":
		return "uphold", "the challenger's own check fails"
	}
	written := entryTime(ch)
	if target.Authority() == memory.AuthorityHuman && !written.IsZero() && now.Sub(written) < challengeGrace {
		return "", "" // a person's fact, recently challenged: give it a week
	}
	return "uphold", "the higher-authority fact stands; nothing contradicts it but the challenge"
}

func entryTime(e memory.Entry) time.Time {
	t, err := time.ParseInLocation(memory.StampFormat, e.Stamp, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// settleChallenges rules on every open challenge in the memory notes among
// docs. With apply false it only decides; with apply true it writes the
// rulings, except "ask", which is returned for the owner.
func (s *Server) settleChallenges(ctx context.Context, docs []dream.Doc, apply bool) []challengeVerdict {
	type located struct {
		note string
		e    memory.Entry
	}
	byID := map[string]located{}
	var open []located
	for _, d := range docs {
		if d.Kind != dream.KindMemoryNote {
			continue
		}
		for _, e := range memory.Parse(d.Body) {
			byID[e.ID] = located{d.Path, e}
			if e.Challenges != "" && !e.Superseded() {
				open = append(open, located{d.Path, e})
			}
		}
	}
	now := vault.Now()
	var out []challengeVerdict
	for _, c := range open {
		t, ok := byID[c.e.Challenges]
		live := ok && t.e.Live(now)
		chState, targetState := "", ""
		if live {
			chState, targetState = anchorState(ctx, c.e.Anchors), anchorState(ctx, t.e.Anchors)
		}
		action, why := decideChallenge(c.e, t.e, live, chState, targetState, now)
		if action == "" {
			continue
		}
		v := challengeVerdict{Note: c.note, ID: c.e.ID, Contested: c.e.Challenges, Action: action, Why: why}
		if apply && action != "ask" {
			if err := s.applyChallengeVerdict(c.note, t.note, v); err != nil {
				v.Action, v.Why = "ask", fmt.Sprintf("could not apply %s: %v", action, err)
			}
		}
		out = append(out, v)
	}
	return out
}

// applyChallengeVerdict writes one ruling: clear drops the flag; uphold and
// concede strike the losing fact through, pointing at the winner, as a
// person's ruling would (settleChallenge), but without marking anyone's fact
// as the person's own, because no person ruled.
func (s *Server) applyChallengeVerdict(note, targetNote string, v challengeVerdict) error {
	clear := func(e *memory.Entry) { e.Challenges = "" }
	if v.Action == "clear" {
		return s.mutateEntry(note, v.ID, clear)
	}
	stamp := vault.Now().Format(memory.StampFormat)
	loserNote, loser, winner := note, v.ID, v.Contested
	if v.Action == "concede" {
		loserNote, loser, winner = targetNote, v.Contested, v.ID
	}
	if err := s.mutateEntry(loserNote, loser, func(e *memory.Entry) {
		e.SupersededBy, e.SupersededAt = winner, stamp
	}); err != nil {
		return err
	}
	return s.mutateEntry(note, v.ID, clear)
}

// challengeLine is a ruling in the report's words.
func challengeLine(v challengeVerdict) string {
	link := "[[" + strings.TrimSuffix(v.Note, ".md") + "]]"
	switch v.Action {
	case "clear":
		return fmt.Sprintf("Closed a challenge in %s (%s): %s", link, v.ID, v.Why)
	case "uphold":
		return fmt.Sprintf("Kept %s over the challenge from %s in %s: %s", v.Contested, v.ID, link, v.Why)
	case "concede":
		return fmt.Sprintf("Replaced %s with %s from %s: %s", v.Contested, v.ID, link, v.Why)
	}
	return fmt.Sprintf("%s in %s challenges %s: %s", v.ID, link, v.Contested, v.Why)
}
