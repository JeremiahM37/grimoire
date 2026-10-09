package connectors

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Live access: the half of a connector that is not a sync.
//
// Syncing copies a source into the vault on a schedule. That is the right shape
// for knowledge that is read often and changes slowly, and the wrong one for
// "what did Dana email me this morning" or "is there an issue for this crash" —
// the vault copy is hours old, or the source was never synced at all. A source
// can therefore also answer live: search the provider now, read one item, and
// (when the operator has allowed it) do something.
//
// Each capability is its own small interface. A source implements the ones it
// can honestly provide and nothing else; the agent layer asks, rather than
// assuming, so IMAP does not need a stub "create_event".

// Hit is one live search result.
type Hit struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Snippet string `json:"snippet,omitempty"`
	URL     string `json:"url,omitempty"`
	Updated string `json:"updated,omitempty"`
	Author  string `json:"author,omitempty"`
}

// Item is one live-read document, already reduced to markdown/plain text.
type Item struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	URL     string `json:"url,omitempty"`
	Updated string `json:"updated,omitempty"`
	Author  string `json:"author,omitempty"`
	// Attachments are LISTED ("name (type, size)"), never fetched.
	Attachments []string `json:"attachments,omitempty"`
}

// Searcher searches the provider now, not the vault copy.
type Searcher interface {
	Search(ctx context.Context, in Input, query string, limit int) ([]Hit, error)
}

// Reader fetches one item by an id a Searcher or a synced note's frontmatter
// (`external_id`) gave out.
type Reader interface {
	Read(ctx context.Context, in Input, id string) (Item, error)
}

// ActionSpec declares one thing an agent may ask a source to do.
type ActionSpec struct {
	// Name is the stable action id, e.g. "post_message". Operators enable it by
	// this name in a connector's `actions` setting.
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Params the call takes. Required ones are checked before anything is
	// queued, so a malformed ask never reaches a human.
	Params []Field `json:"params"`
	// Scopes the credential needs for it; documented in docs/CONNECTORS.md and
	// requested by `grimoire connect --allow`. Empty when the read scope is enough.
	Scopes []string `json:"scopes,omitempty"`
	// Irreversible marks actions that cannot be taken back (a sent message, a
	// posted comment) as opposed to a draft the owner can still discard.
	Irreversible bool `json:"irreversible,omitempty"`
}

// ActionResult is what an executed action reports back. It never carries a
// credential; URL and ID are what the agent needs to refer to the result.
type ActionResult struct {
	ID      string `json:"id,omitempty"`
	URL     string `json:"url,omitempty"`
	Message string `json:"message,omitempty"`
}

// Actor performs actions. Act is called only by the action service, after the
// action class is enabled and (by default) a person has approved the call.
type Actor interface {
	Actions() []ActionSpec
	Act(ctx context.Context, in Input, action string, params map[string]string) (ActionResult, error)
}

// Trust classes for a connector's content. See Document.Own.
const (
	TrustOwn      = "own"
	TrustTeam     = "team"
	TrustExternal = "external"
)

// TrustOf reads a connector's configured trust class. Anything unset or
// unrecognised is external: a typo must not promote a source.
func TrustOf(c Config) string {
	switch strings.ToLower(c.Get("trust")) {
	case TrustOwn:
		return TrustOwn
	case TrustTeam:
		return TrustTeam
	}
	return TrustExternal
}

// commonFields are settings every kind accepts; appended by Kinds() so each
// source need not repeat them.
func commonFields() []Field {
	return []Field{
		{Name: "trust", Label: "Trust", Placeholder: "external",
			Help: "own, team or external (default). own lets documents the account " +
				"owner demonstrably wrote (their sent mail, Drive files they own and last " +
				"edited) feed recall and automatic context; everything else from this " +
				"source stays retrieval-only and fenced as untrusted."},
		{Name: "actions", Label: "Enabled agent actions", Placeholder: "create_draft",
			Help: "Comma-separated action names agents may request through source_act. " +
				"Empty (default) means read-only: nothing can be done through this source."},
		{Name: "action_approval", Label: "Action approval", Placeholder: "each",
			Help: "each (default): every call waits for the owner. none: enabled actions run immediately."},
		{Name: "action_rate", Label: "Actions per hour", Placeholder: "10"},
	}
}

// validateCommon checks the cross-kind settings.
func validateCommon(kind string, cfg Config) error {
	if t := strings.ToLower(cfg.Get("trust")); t != "" && t != TrustOwn && t != TrustTeam && t != TrustExternal {
		return fmt.Errorf("%w: trust must be own, team or external", ErrConfig)
	}
	if a := strings.ToLower(cfg.Get("action_approval")); a != "" && a != "each" && a != "none" {
		return fmt.Errorf("%w: action_approval must be each or none", ErrConfig)
	}
	s, err := Get(kind)
	if err != nil {
		return err
	}
	enabled := splitList(cfg.Get("actions"))
	if len(enabled) == 0 {
		return nil
	}
	actor, ok := s.(Actor)
	if !ok {
		return fmt.Errorf("%w: %s has no agent actions", ErrConfig, kind)
	}
	known := map[string]bool{}
	for _, a := range actor.Actions() {
		known[a.Name] = true
	}
	for _, name := range enabled {
		if !known[name] {
			names := make([]string, 0, len(known))
			for k := range known {
				names = append(names, k)
			}
			sort.Strings(names)
			return fmt.Errorf("%w: unknown action %q for %s (available: %s)",
				ErrConfig, name, kind, strings.Join(names, ", "))
		}
	}
	return nil
}
