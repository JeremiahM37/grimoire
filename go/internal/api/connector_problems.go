package api

import (
	"fmt"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/connectors"
)

// Connectors that need the owner, as plain problems.
//
// The health endpoint and the owner's inbox both read from here, so they cannot
// disagree about what is broken. A problem says what is wrong, since when, and
// the one thing the owner has to do about it.

// Problem is something the owner needs to act on. Source says where it came
// from ("connector" for now), so an inbox can group problems from several
// places without guessing.
type Problem struct {
	Source string    `json:"source"`
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	Detail string    `json:"detail"`
	Since  time.Time `json:"since"`
	Action string    `json:"action"`
}

// connectorEntry is one unhealthy connector as /api/health lists it. It names
// the account by the connector's own label, never by a credential.
type connectorEntry struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Account   string `json:"account"`
	Since     string `json:"since"`
	LastError string `json:"last_error"`
}

// connectorReport is the connectors block of /api/health.
type connectorReport struct {
	OK          int              `json:"ok"`
	Failing     []connectorEntry `json:"failing"`
	NeedsReauth []connectorEntry `json:"needs_reauth"`
}

// ConnectorProblems returns what the owner must act on for connectors.
func (s *Server) ConnectorProblems() []Problem {
	_, problems := s.connectorState()
	return problems
}

// connectorState reads the configured connectors and their health.
func (s *Server) connectorState() (connectorReport, []Problem) {
	report := connectorReport{Failing: []connectorEntry{}, NeedsReauth: []connectorEntry{}}
	if s.Connectors == nil {
		return report, nil
	}
	list, err := s.Connectors.List()
	if err != nil {
		return report, nil
	}
	health, err := s.Connectors.AllHealth()
	if err != nil {
		return report, nil
	}
	return buildConnectorState(list, health)
}

// buildConnectorState is the pure part: classify each enabled connector from
// its health and say what to do about each one that is not fine.
func buildConnectorState(list []connectors.Connector, health map[string]connectors.Health) (connectorReport, []Problem) {
	report := connectorReport{Failing: []connectorEntry{}, NeedsReauth: []connectorEntry{}}
	var problems []Problem
	for _, c := range list {
		if !c.Enabled {
			continue
		}
		h := health[c.ID]
		since, _ := time.Parse(time.RFC3339, h.Since)
		entry := connectorEntry{ID: c.ID, Kind: c.Kind, Account: c.Name,
			Since: h.Since, LastError: h.LastError}
		switch h.State {
		case connectors.StateNeedsReauth:
			report.NeedsReauth = append(report.NeedsReauth, entry)
			problems = append(problems, Problem{
				Source: "connector", ID: c.ID, Title: connectorTitle(c),
				Detail: fmt.Sprintf("needs you to sign in again since %s", since.Format("2006-01-02")),
				Since:  since, Action: reauthAction(c.Kind),
			})
		case connectors.StateFailing:
			report.Failing = append(report.Failing, entry)
			problems = append(problems, Problem{
				Source: "connector", ID: c.ID, Title: connectorTitle(c),
				Detail: fmt.Sprintf("has failed %d runs in a row since %s: %s",
					h.Failures, since.Format("2006-01-02"), h.LastError),
				Since:  since,
				Action: "Open Settings → Connectors to read the last error, fix its cause, then press Run to retry",
			})
		default:
			report.OK++
		}
	}
	if problems == nil {
		problems = []Problem{}
	}
	return report, problems
}

// connectorTitle names a connector the way the owner knows it, with the
// credential it signs in with when it has one: "Gmail (google-oauth)".
func connectorTitle(c connectors.Connector) string {
	if c.Secret == "" {
		return c.Name
	}
	return fmt.Sprintf("%s (%s)", c.Name, c.Secret)
}

// reauthAction is the instruction for a dead sign-in. Google's refresh tokens
// expire every seven days while the OAuth app is in Testing, so for Google the
// durable fix is publishing the app, not reconnecting.
func reauthAction(kind string) string {
	if connectors.ProviderOf(kind) == "google" {
		return "Reconnect at Settings → Connectors (Google sign-in expired; to stop this weekly, " +
			"publish the OAuth app to In production in Google Cloud Console)"
	}
	return "Reconnect at Settings → Connectors (the sign-in expired or was revoked)"
}
