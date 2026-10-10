package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// BackendClaudeCLI runs a completion through a locally installed, already
// logged-in `claude` command instead of the HTTP API. It exists for operators
// who have Claude Code but no API key, and for benchmarks that must use exactly
// the model route the rest of their tooling uses.
//
// It is opt-in (`llm=claude-cli`) and never auto-selected. The command runs
// with no tools, no MCP servers and no settings sources, from an empty
// scratch directory, so a completion cannot read the machine it runs on.
const BackendClaudeCLI = "claude-cli"

// CLIReply carries what the command reported besides the text.
type CLIReply struct {
	CostUSD float64
	Millis  int64
}

// completeClaudeCLI runs one completion through the claude command.
func (c *Client) completeClaudeCLI(ctx context.Context, prompt string, o CompleteOpts) (Completion, CLIReply, error) {
	bin := c.get("claude_cli_path")
	if bin == "" {
		bin = "claude"
	}
	model := c.model()
	if model == "qwen3.5:4b" {
		model = "claude-sonnet-5"
	}
	system := strings.TrimSpace(o.System)
	if o.JSON {
		system = strings.TrimSpace(system + "\n\nRespond with a single JSON value and nothing else.")
	}
	if system == "" {
		system = "You are a careful assistant."
	}
	dir, err := os.MkdirTemp("", "grimoire-claude-cli-")
	if err != nil {
		return Completion{}, CLIReply{}, err
	}
	defer os.RemoveAll(dir)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		cmd := exec.CommandContext(cctx, bin, "-p", "--model", model, "--output-format", "json",
			"--strict-mcp-config", "--setting-sources", "", "--max-turns", "1", "--tools", "",
			"--system-prompt", system)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(prompt)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		started := time.Now()
		err := cmd.Run()
		cancel()
		var r struct {
			Result  string  `json:"result"`
			IsError bool    `json:"is_error"`
			Cost    float64 `json:"total_cost_usd"`
			Ms      int64   `json:"duration_ms"`
			Usage   struct {
				In      int `json:"input_tokens"`
				CacheIn int `json:"cache_read_input_tokens"`
				CacheWr int `json:"cache_creation_input_tokens"`
				Out     int `json:"output_tokens"`
			} `json:"usage"`
		}
		if jerr := json.Unmarshal(stdout.Bytes(), &r); err == nil && jerr == nil && !r.IsError {
			tok := usage.FromAnthropic(r.Usage.In+r.Usage.CacheIn+r.Usage.CacheWr, r.Usage.Out)
			c.observe(BackendClaudeCLI, model, tok, started, nil)
			return Completion{Text: strings.TrimSpace(r.Result), FinishReason: "stop", Usage: tok, Model: model},
				CLIReply{CostUSD: r.Cost, Millis: r.Ms}, nil
		}
		switch {
		case err != nil:
			lastErr = fmt.Errorf("claude: %v: %s", err, strings.TrimSpace(firstN(stderr.String()+stdout.String(), 300)))
		case r.IsError:
			lastErr = fmt.Errorf("claude: %s", firstN(r.Result, 300))
		default:
			lastErr = fmt.Errorf("claude: unreadable reply")
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(10 * time.Second)
	}
	c.observe(BackendClaudeCLI, model, usage.Tokens{}, time.Now(), lastErr)
	return Completion{}, CLIReply{}, lastErr
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// CompleteCLI is CompleteWith on the claude command that also returns the
// command's own cost and latency, for callers that account for both.
func (c *Client) CompleteCLI(ctx context.Context, prompt string, o CompleteOpts) (Completion, CLIReply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.completeClaudeCLI(ctx, prompt, o)
}
