package connectors

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestConnectorRoundTrip(t *testing.T) {
	s := NewStore(testDB(t))
	c := Connector{ID: "a", Kind: "rss", Name: "Changelog",
		Config: Config{"url": "https://example.com/feed.xml"},
		Prefix: "connectors/feeds", Interval: 900, Enabled: true,
		Created: "2026-08-19T00:00:00Z"}
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.Get("url") != c.Config.Get("url") || !got.Enabled || got.Interval != 900 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v (%v)", list, err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("a"); err == nil {
		t.Fatal("a deleted connector still resolves")
	}
}

func TestDueRespectsIntervalAndEnabled(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		c    Connector
		want bool
	}{
		{"never run", Connector{Enabled: true, Interval: 300}, true},
		{"due", Connector{Enabled: true, Interval: 300,
			LastRun: now.Add(-10 * time.Minute).Format(time.RFC3339)}, true},
		{"not yet", Connector{Enabled: true, Interval: 3600,
			LastRun: now.Add(-10 * time.Minute).Format(time.RFC3339)}, false},
		{"manual only", Connector{Enabled: true, Interval: 0}, false},
		{"disabled", Connector{Enabled: false, Interval: 60}, false},
	}
	for _, c := range cases {
		if got := c.c.Due(now); got != c.want {
			t.Errorf("%s: Due = %v, want %v", c.name, got, c.want)
		}
	}
}
