package api

import (
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

func TestDecideChallenge(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	old := now.Add(-30 * 24 * time.Hour).Format(memory.StampFormat)
	fresh := now.Add(-time.Hour).Format(memory.StampFormat)
	person := memory.Entry{ID: "p", Text: "the grimoire service listens on port 9111", Human: true}
	agent := func(text, stamp string) memory.Entry {
		return memory.Entry{ID: "a", Text: text, Stamp: stamp, Challenges: "p"}
	}
	same := "the grimoire service listens on port 9112"
	cases := []struct {
		name         string
		ch, target   memory.Entry
		live         bool
		chS, targetS string
		want         string
	}{
		{"contested fact gone", agent(same, old), person, false, "", "", "clear"},
		{"event against a fact", memory.Entry{ID: "a", Text: same, Event: true, Stamp: old}, person, true, "", "", "clear"},
		{"different subjects", agent("the backup timer runs nightly at three", old), person, true, "", "", "clear"},
		{"evidence for the challenger", agent(same, old), person, true, "pass", "fail", "concede"},
		{"person's check fails, nothing confirms agent", agent(same, old), person, true, "", "fail", "ask"},
		{"both checks pass", agent(same, old), person, true, "pass", "pass", "ask"},
		{"person's check passes", agent(same, old), person, true, "", "pass", "uphold"},
		{"challenger's check fails", agent(same, old), person, true, "fail", "", "uphold"},
		{"recent challenge to a person waits", agent(same, fresh), person, true, "", "", ""},
		{"old challenge to a person is upheld", agent(same, old), person, true, "", "", "uphold"},
	}
	for _, c := range cases {
		got, _ := decideChallenge(c.ch, c.target, c.live, c.chS, c.targetS, now)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
