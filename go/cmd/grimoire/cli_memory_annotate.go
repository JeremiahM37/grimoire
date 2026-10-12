package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/mcp"
)

// `grimoire memory annotate` is a client of POST /api/memory/annotate. It reads
// a JSON file of {"items":[{id, path, event?, host?, anchors?}]} and changes
// only the trailers of those facts. It is a dry run unless --apply is given.
//
// The write goes to the running server when one answers (postLive), because
// the server is the one process that should write the vault while it runs. The
// CLI opens the vault itself only when nothing answers at the address.

// errNoServer means nothing accepted a connection at the server address. It is
// the one case in which a command may open the vault in-process.
var errNoServer = errors.New("no grimoire server answered")

// liveBase is the address of the running server, as a CLI command reaches it:
// GRIMOIRE_URL when set, else loopback on GRIMOIRE_PORT (default 9111).
func liveBase() string {
	if u := strings.TrimSpace(os.Getenv(mcp.EnvURL)); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://127.0.0.1:" + envOr("GRIMOIRE_PORT", "9111")
}

// postLive posts one JSON request to the running server as the local operator
// and returns the status and body it answered with. It returns errNoServer only
// when the connection was refused or could not be made. A server that answered
// with an error, or a request that timed out after connecting, is an answer,
// and the caller must not run the same write a second time.
func postLive(path string, body any) (int, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest("POST", liveBase()+path, strings.NewReader(string(raw)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if tok := os.Getenv("GRIMOIRE_AUTH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if adm := os.Getenv("GRIMOIRE_ADMIN_TOKEN"); adm != "" {
		req.Header.Set("X-Grimoire-Admin", adm)
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, "", errNoServer
		}
		return 0, "", fmt.Errorf("grimoire at %s: %w", liveBase(), err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(out), nil
}

// reportPath says which path a command took. JSON output on stdout stays the
// server's answer unchanged, so the note goes to stderr there.
func reportPath(jsonOut bool, live bool) {
	msg := "via live server at " + liveBase()
	if !live {
		msg = "in-process: no server answered at " + liveBase() + ", vault opened directly"
	}
	if jsonOut {
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	fmt.Println(msg)
}

// memoryAnnotateCmd applies or previews an annotation file. --json prints the
// server's answer unchanged.
func memoryAnnotateCmd(args []string) int {
	file, ok := flagValue(args, "--file")
	if !ok || file == "" {
		return fail("annotate needs --file FILE.json (see grimoire memory help)")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fail("annotate: %v", err)
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Items) == 0 {
		return fail("annotate: %s must be JSON with a non-empty items list", file)
	}
	apply := hasFlag(args, "--apply")
	req := map[string]any{"items": body.Items, "dry_run": !apply}
	jsonOut := hasFlag(args, "--json")

	status, resp, err := postLive("/api/memory/annotate", req)
	live := err == nil
	if errors.Is(err, errNoServer) {
		e, oerr := openEnv()
		if oerr != nil {
			return fail("annotate: %v", oerr)
		}
		defer e.close()
		// The CLI is the local operator; the server's annotate gate wants a loopback peer.
		status, resp = e.callFrom("127.0.0.1:0", "POST", "/api/memory/annotate", req)
	} else if err != nil {
		return fail("annotate: %v", err)
	}
	if status != http.StatusOK {
		return fail("annotate failed: %s", resp)
	}
	if jsonOut {
		reportPath(true, live)
		fmt.Println(resp)
		return 0
	}
	reportPath(false, live)
	var out struct {
		NotesWritten int `json:"notes_written"`
		Items        []struct {
			ID      string `json:"id"`
			Changed bool   `json:"changed"`
			Error   string `json:"error"`
			Anchors []struct {
				Anchor  string `json:"anchor"`
				Result  string `json:"result"`
				Message string `json:"message"`
			} `json:"anchors"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return fail("annotate: unreadable response: %v", err)
	}
	changed, errs := 0, 0
	for _, it := range out.Items {
		switch {
		case it.Error != "":
			errs++
			fmt.Printf("  %s  error: %s\n", it.ID, it.Error)
		case it.Changed:
			changed++
			fmt.Printf("  %s  changed\n", it.ID)
		default:
			fmt.Printf("  %s  unchanged\n", it.ID)
		}
		for _, a := range it.Anchors {
			fmt.Printf("      %s  %s  %s\n", a.Anchor, a.Result, a.Message)
		}
	}
	fmt.Printf("items %d  changed %d  errors %d  notes written %d\n", len(out.Items), changed, errs, out.NotesWritten)
	if !apply {
		fmt.Println("dry run: nothing was written (add --apply to write)")
	}
	return 0
}

// skipReasonLines formats the skipped-anchor counts of the anchors report in a
// fixed order, so the output does not change from run to run.
func skipReasonLines(reasons map[string]int) []string {
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s %d", k, reasons[k]))
	}
	return out
}
