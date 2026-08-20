// Package connectors pulls documents from systems that already hold them.
//
// A context server that only knows what you typed into it knows very little.
// The knowledge a team actually runs on is in Slack threads, Confluence pages,
// Jira tickets and Drive documents, and none of it is going to be retyped.
//
// The design constraint here is Grimoire's own: notes are plain markdown files
// that outlive the app. So a connector does not build a parallel document
// store — it writes markdown into the vault, under a prefix, with provenance
// in the frontmatter. Everything downstream (search, retrieval, spaces, the
// editor, sync) then works on connector documents for free, because they are
// notes. You can also read them with `cat`, and if Grimoire disappears
// tomorrow the pulled knowledge is still there.
//
// Configuration is explicit and per-connector: an operator names the source,
// the credential, the destination prefix and the schedule. There is no
// discovery and no default set of integrations — a connector exists because
// somebody configured it, which is the difference between a tool you can reason
// about and one that quietly ingests a company.
package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Document is one item pulled from a source, already reduced to markdown.
type Document struct {
	// ExternalID is stable per source item; it is what makes a re-sync an
	// update rather than a duplicate.
	ExternalID string
	Title      string
	Body       string
	URL        string
	Updated    string
	Author     string
	// Extra frontmatter, e.g. channel, project, labels.
	Meta map[string]string
}

// Page is one batch of documents plus the cursor to resume from.
type Page struct {
	Docs   []Document
	Cursor string
	// More reports that the source has further pages ready now; the runner
	// keeps going rather than waiting for the next scheduled sync.
	More bool
	// Complete marks a page as a FULL enumeration of the source rather than
	// "what changed since the cursor".
	//
	// This is what makes deletion detectable at all. An incremental sync asks
	// "what moved?", and a document that was deleted did not move — it is
	// indistinguishable from one that was simply not touched. Only a source
	// that just listed everything can say "and nothing else exists", which is
	// the claim this flag makes. Setting it when the page is partial would
	// delete every document the page did not happen to include.
	Complete bool
	// Seen lists every external id the source holds, when a connector can
	// enumerate cheaply but does not want to return the bodies. Used with
	// Complete.
	Seen []string
}

// Config is a connector's settings: free-form per kind, validated by the kind.
type Config map[string]string

func (c Config) Get(key string) string { return strings.TrimSpace(c[key]) }

// Source is one kind of system Grimoire can pull from.
type Source interface {
	// Kind is the stable identifier stored in the database.
	Kind() string
	// Describe declares what an operator must configure, so the console can
	// render a form and the API can validate without knowing any specifics.
	Describe() Kind
	// Fetch returns documents changed since the cursor. An empty cursor means
	// a first, full sync.
	Fetch(ctx context.Context, in Input) (Page, error)
}

// Input is everything a fetch needs.
type Input struct {
	Config Config
	// Secret is the credential value, resolved from the vault by the runner.
	// A connector never sees where it came from, and the value is never
	// written to the database or returned by the API.
	Secret string
	Cursor string
	Client *http.Client
	// Limit bounds one fetch, so a first sync of a large source makes progress
	// in bounded steps rather than one enormous request.
	Limit int
}

// Field is one configurable setting.
type Field struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Kind describes a source to an operator.
type Kind struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Help explains what to configure and where to get the credential, since
	// that is the part every integration guide gets vague about.
	Help string `json:"help"`
	// SecretHelp says what the credential is and how to obtain it. Empty when
	// the source needs none.
	SecretHelp string  `json:"secret_help,omitempty"`
	Fields     []Field `json:"fields"`
	// DefaultPrefix is where documents land unless the operator says otherwise.
	DefaultPrefix string `json:"default_prefix"`
}

var (
	ErrUnknownKind = errors.New("unknown connector kind")
	ErrConfig      = errors.New("connector configuration")
)

// registry holds every available source kind.
var registry = map[string]Source{}

// Register adds a source kind. Called from each source's init.
func Register(s Source) { registry[s.Kind()] = s }

// Get returns a source by kind.
func Get(kind string) (Source, error) {
	s, ok := registry[strings.ToLower(strings.TrimSpace(kind))]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return s, nil
}

// Kinds lists every available source, for the configuration UI.
func Kinds() []Kind {
	out := make([]Kind, 0, len(registry))
	for _, s := range registry {
		out = append(out, s.Describe())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Validate checks a configuration against its kind's declared fields, so a
// misconfiguration is reported when it is saved rather than at 3am when the
// scheduled sync fails.
func Validate(kind string, cfg Config) error {
	s, err := Get(kind)
	if err != nil {
		return err
	}
	for _, f := range s.Describe().Fields {
		if f.Required && cfg.Get(f.Name) == "" {
			return fmt.Errorf("%w: %s is required", ErrConfig, f.Label)
		}
	}
	return nil
}

// missing reports a configuration error for a required field.
func missing(field string) error {
	return fmt.Errorf("%w: %s is required", ErrConfig, field)
}

// rfc3339 formats a time the way every source here wants it.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
