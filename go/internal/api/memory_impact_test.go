package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryImpactEndpointAndRetells(t *testing.T) {
	s, h := testServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Profiles live under XDG_CONFIG_HOME when it is set (CI runners set it).
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg := filepath.Join(home, ".config", "grimoire", "agents")
	os.MkdirAll(cfg, 0o755)
	os.MkdirAll(filepath.Join(home, "logs"), 0o755)
	os.WriteFile(filepath.Join(cfg, "myagent.json"), []byte(`{"hooks":{"file":""},"transcripts":{"glob":"~/logs/*.jsonl","format":"generic-jsonl",`+
		`"map":{"session":"s","time":"t","role":"r","text":"x","tool":"tool","tool_target":"cmd","tool_error":"err"}}}`), 0o644)
	var lines []string
	base := time.Now().AddDate(0, 0, -20)
	for d := 0; d < 12; d++ {
		ts := base.AddDate(0, 0, d).Unix()
		lines = append(lines, fmt.Sprintf(`{"s":"s%d","t":%d,"r":"user","x":"deploy kestrel to staging cluster first never production migration"}`, d, ts))
		lines = append(lines, fmt.Sprintf(`{"s":"s%d","t":%d,"tool":"bash","cmd":"kestrel deploy","err":%d}`, d, ts+30, map[bool]int{true: 1, false: 0}[d < 6]))
	}
	os.WriteFile(filepath.Join(home, "logs", "a.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	store := t.TempDir()
	mid := base.AddDate(0, 0, 6).Format("2006-01-02")
	os.WriteFile(filepath.Join(store, "reference_kestrel.md"), []byte("---\nname: Kestrel deploy\ndescription: deploy kestrel to staging cluster first never production migration\nmetadata:\n  created: "+mid+"\n---\nbody\n"), 0o644)
	if err := s.Settings.Update(map[string]string{"memory_canonical_dir": store}); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", "/api/memory/impact?since=60d&min=3", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var out struct {
		Report struct {
			Caveat   string
			Sessions int `json:"sessions_considered"`
			Items    []struct {
				Name string
				All  struct{ Verdict string }
			}
		}
	}
	decode(t, w, &out)
	if out.Report.Sessions != 12 || len(out.Report.Items) != 1 || !strings.Contains(out.Report.Caveat, "Correlation only") {
		t.Fatalf("%+v", out)
	}
	if bad := do(t, h, "GET", "/api/memory/impact?since=banana", nil); bad.Code != 400 {
		t.Errorf("bad since = %d", bad.Code)
	}

	w = do(t, h, "GET", "/api/memory/impact?retells=1&cutoff=2026-10-09", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var rt struct {
		Retells struct {
			Judge  string
			Weeks  []struct{ Week string }
			Before struct {
				Prompts int `json:"user_prompts"`
			}
		}
	}
	decode(t, w, &rt)
	if rt.Retells.Judge != "strict" || len(rt.Retells.Weeks) == 0 || rt.Retells.Before.Prompts+0 == 0 {
		t.Fatalf("%s", w.Body)
	}
	if do(t, h, "GET", "/api/memory/impact?retells=1&cutoff=nope", nil).Code != 400 {
		t.Error("bad cutoff must be 400")
	}
}
