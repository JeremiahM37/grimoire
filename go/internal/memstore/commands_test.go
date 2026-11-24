package memstore

import (
	"strings"
	"testing"
)

func TestCondenseCommandsKeepsTheProse(t *testing.T) {
	note := "Tailscale has a full REST API that mirrors the admin console.\n\n" +
		"**Credentials file:** `/srv/keys/ts.yaml` (600 perms)\n\n" +
		"1. **OAuth client** (preferred). Exchange `client_id` + `client_secret` for a bearer token.\n\n" +
		"   ```bash\n   CID=$(grep '^ client_id:' /srv/keys/ts.yaml | awk '{print $2}')\n" +
		"   curl -H \"Authorization: Bearer $BEARER\" https://api.tailscale.com/api/v2/tailnet/-/dns\n   ```\n\n" +
		"2. **Personal access token** (fallback). Use directly as Bearer."
	got, n := CondenseCommands(note)
	if n != 1 || strings.Contains(got, "CID=") || strings.Contains(got, "curl") {
		t.Fatalf("%d %q", n, got)
	}
	for _, keep := range []string{"full REST API", "ts.yaml", "Personal access token"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("lost %q: %q", keep, got)
		}
	}
}

func TestCondenseCommandsDropsNumberedCommandSteps(t *testing.T) {
	note := "Use when: give Codex the memory hook.\n\nSteps, as run:\n\n" +
		"1. export PATH=$PATH:/usr/local/go/bin; cd /mnt/bulk/grimoire-am/go && go build -o <binary> ./cmd/grimoire\n" +
		"2. B=/mnt/bulk/admin-backups/x && mkdir -p $B && cp -a ~/.codex/hooks.json $B/\n" +
		"3. grep -c grimoire ~/.codex/config.toml\n" +
		"Run `systemctl restart grimoire` afterwards, then check `docs/MEMORY.md`."
	got, n := CondenseCommands(note)
	if n != 4 {
		t.Fatalf("removed %d: %q", n, got)
	}
	if !strings.Contains(got, "Use when: give Codex the memory hook.") || !strings.Contains(got, "`docs/MEMORY.md`") {
		t.Fatalf("%q", got)
	}
	if strings.Contains(got, "go build") || strings.Contains(got, "systemctl") {
		t.Fatalf("%q", got)
	}
}

func TestLooksLikeCommandLeavesSentencesAlone(t *testing.T) {
	for _, prose := range []string{
		"Go lives at /usr/local/go/bin, not on the session PATH.",
		"git history is rewritten only when the user asks.",
		"Use the grimoire service for memory.",
	} {
		if LooksLikeCommand(prose) {
			t.Errorf("prose taken for a command: %q", prose)
		}
	}
}

func TestCondenseCommandsFindsCommandsInsideSentences(t *testing.T) {
	note := "## Run and test\n\nInstall: go install github.com/x/y/cmd/y@latest. Source build: npm ci --prefix frontend; npm run build --prefix frontend. Run: GRIMOIRE_VAULT=<vault> grimoire serve.\n" +
		"2. python3 qeval.py run mmlu <port> 600 <out>.json\n" +
		"Releases are cut from main. The vault lives in /home/admin/notes: back it up first."
	got, n := CondenseCommands(note)
	for _, gone := range []string{"go install", "npm ci", "npm run", "grimoire serve", "qeval"} {
		if strings.Contains(got, gone) {
			t.Fatalf("%q left in: %q", gone, got)
		}
	}
	for _, keep := range []string{"Install:", "Source build:", "Releases are cut from main.", "back it up first"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("lost %q: %q", keep, got)
		}
	}
	if n != 5 {
		t.Fatalf("removed %d: %q", n, got)
	}
}
