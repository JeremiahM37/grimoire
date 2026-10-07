package bank

import (
	"errors"
	"sort"
	"strings"
)

// Bank templates are a bank's configuration as data: profile fields,
// settings, standing questions (mental models) and directives. A template is
// applied by importing it, and any bank's configuration can be exported as
// one — to copy a setup between banks or instances, or keep it in git.
//
// Import is additive. It sets what the manifest names and leaves everything
// else alone: no setting, model or directive is removed, a model's answer is
// never touched (only its question and settings), and a profile field is
// changed only when the manifest carries a value for it.

// TemplateBank is the profile part of a manifest.
type TemplateBank struct {
	Name          string            `json:"name,omitempty"`
	Mission       string            `json:"mission,omitempty"`
	RetainMission string            `json:"retain_mission,omitempty"`
	Disposition   *Disposition      `json:"disposition,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	Config        map[string]string `json:"config,omitempty"`
}

// TemplateModel is a mental model in a manifest. Matched by id on import.
type TemplateModel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Question  string   `json:"question"`
	Tags      []string `json:"tags,omitempty"`
	Refresh   string   `json:"refresh,omitempty"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Budget    string   `json:"budget,omitempty"`
	FactTypes []string `json:"fact_types,omitempty"`
}

// TemplateDirective is a directive in a manifest. Matched by name on import
// (or by text when it has none).
type TemplateDirective struct {
	Name     string   `json:"name,omitempty"`
	Text     string   `json:"text"`
	Tags     []string `json:"tags,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Inactive bool     `json:"inactive,omitempty"`
}

// Manifest is a bank template.
type Manifest struct {
	Version      string              `json:"version"`
	Bank         *TemplateBank       `json:"bank,omitempty"`
	MentalModels []TemplateModel     `json:"mental_models,omitempty"`
	Directives   []TemplateDirective `json:"directives,omitempty"`
}

// Template is a built-in template.
type Template struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Manifest    Manifest `json:"manifest"`
}

func disp(s, l, e int) *Disposition { return &Disposition{Skepticism: s, Literalism: l, Empathy: e} }

// BuiltinTemplates are the templates shipped with Grimoire.
var BuiltinTemplates = []Template{
	{ID: "assistant", Name: "Personal assistant",
		Description: "A general assistant that remembers the user: who they are, what they prefer, what they are working on and what they asked for.",
		Manifest: Manifest{Version: "1", Bank: &TemplateBank{
			Mission:       "Help the user by remembering who they are, the people in their life, their preferences, commitments and ongoing plans.",
			RetainMission: "Keep facts about the user and the people around them, their preferences, plans, commitments, routines and decisions. Skip small talk.",
			Disposition:   disp(3, 3, 4),
			Config:        map[string]string{"consolidation": "auto"},
		}, MentalModels: []TemplateModel{
			{ID: "user-profile", Name: "User profile", Question: "Who is the user: background, work, relationships and what matters to them?"},
			{ID: "preferences", Name: "Preferences", Question: "What does the user prefer and dislike, in how they work, communicate and live?"},
			{ID: "open-commitments", Name: "Open commitments", Question: "What has the user committed to or planned that is not yet done, with dates?"},
		}}},
	{ID: "coding-agent", Name: "Coding agent",
		Description: "Memory for an agent working in one codebase: decisions and their reasons, conventions, the developer's preferences and recurring review feedback.",
		Manifest: Manifest{Version: "1", Bank: &TemplateBank{
			Mission:       "Support work on this codebase: recall the project's decisions, conventions and constraints, and how the developer likes things done.",
			RetainMission: "Keep technical decisions and their reasons, conventions, architecture, build and test commands, constraints, bugs found and their fixes, and the developer's stated preferences. Skip routine tool output.",
			Disposition:   disp(3, 5, 2),
			Config:        map[string]string{"consolidation": "auto", "observations_mission": "Track project facts: architecture, conventions, decisions with reasons, known pitfalls, and the developer's preferences."},
		}, MentalModels: []TemplateModel{
			{ID: "project-context", Name: "Project context", Question: "What is this project, how is it built and tested, and what are its main components and constraints?"},
			{ID: "developer-preferences", Name: "Developer preferences", Question: "How does the developer want code written, reviewed and committed?"},
			{ID: "review-patterns", Name: "Review patterns", Question: "What feedback recurs in reviews of changes to this project, and what mistakes keep coming back?"},
		}, Directives: []TemplateDirective{
			{Name: "Cite decisions", Text: "When an answer rests on a past decision, say when it was made and why."},
		}}},
	{ID: "support", Name: "Customer support",
		Description: "Memory for a support agent: each customer's history, issues and how they were resolved, and how the customer felt about it.",
		Manifest: Manifest{Version: "1", Bank: &TemplateBank{
			Mission:       "Help support staff serve each customer well, knowing their history, open issues and what resolved similar problems before.",
			RetainMission: "Keep customer details, products and plans, reported issues with dates, what was tried, what resolved them, and how the customer felt. Skip pleasantries.",
			Disposition:   disp(2, 3, 5),
			Config:        map[string]string{"consolidation": "auto"},
		}, MentalModels: []TemplateModel{
			{ID: "open-issues", Name: "Open issues", Question: "Which customer issues are unresolved, since when, and what has been tried?"},
			{ID: "known-fixes", Name: "Known fixes", Question: "Which problems keep recurring and what resolved them?"},
		}, Directives: []TemplateDirective{
			{Name: "No internal notes", Text: "Never repeat internal-only notes or other customers' details in an answer meant for a customer."},
		}}},
	{ID: "research", Name: "Research assistant",
		Description: "Memory for research: sources, claims with where they came from, open questions, and how the user's thinking developed.",
		Manifest: Manifest{Version: "1", Bank: &TemplateBank{
			Mission:       "Support research by keeping track of sources, the claims they make, open questions and how conclusions developed.",
			RetainMission: "Keep claims with their sources, numbers with units and dates, methods, open questions, disagreements between sources, and the user's interests. Skip chatter.",
			Disposition:   disp(4, 4, 2),
			Config:        map[string]string{"consolidation": "auto"},
		}, MentalModels: []TemplateModel{
			{ID: "open-questions", Name: "Open questions", Question: "Which research questions are still open, and what evidence bears on each?"},
			{ID: "key-findings", Name: "Key findings", Question: "What are the main findings so far, with the sources that support them?"},
		}, Directives: []TemplateDirective{
			{Name: "Attribute claims", Text: "Attribute every claim to its source, and say when sources disagree."},
		}}},
	{ID: "plain-retrieval", Name: "Plain retrieval",
		Description: "A bank with no model in the loop: content is stored chunk by chunk and recalled by meaning, words and time.",
		Manifest: Manifest{Version: "1", Bank: &TemplateBank{
			Config: map[string]string{"retain_extraction_mode": "chunks", "consolidation": "off", "enable_graph": "false"},
		}}},
}

// BuiltinTemplate returns one built-in template.
func BuiltinTemplate(id string) (*Template, bool) {
	for i := range BuiltinTemplates {
		if BuiltinTemplates[i].ID == id {
			t := BuiltinTemplates[i]
			return &t, true
		}
	}
	return nil, false
}

// Validate checks a manifest before anything is written.
func (m *Manifest) Validate() error {
	if m.Version == "" {
		m.Version = "1"
	}
	if m.Version != "1" {
		return invalid("manifest version %q is not supported (want \"1\")", m.Version)
	}
	if m.Bank != nil {
		if m.Bank.Disposition != nil {
			if err := ValidateDisposition(*m.Bank.Disposition); err != nil {
				return invalid("%v", err)
			}
		}
		if err := ValidateConfig(m.Bank.Config); err != nil {
			return invalid("%v", err)
		}
	}
	ids := map[string]bool{}
	for i, mm := range m.MentalModels {
		if !ValidModelID(mm.ID) {
			return invalid("mental_models[%d]: id %q is not a valid model id", i, mm.ID)
		}
		if ids[mm.ID] {
			return invalid("mental_models[%d]: duplicate id %q", i, mm.ID)
		}
		ids[mm.ID] = true
		if strings.TrimSpace(mm.Question) == "" {
			return invalid("mental_models[%d]: question must not be empty", i)
		}
	}
	names := map[string]bool{}
	for i, d := range m.Directives {
		if strings.TrimSpace(d.Text) == "" {
			return invalid("directives[%d]: text must not be empty", i)
		}
		key := strings.ToLower(directiveKey(d.Name, d.Text))
		if names[key] {
			return invalid("directives[%d]: duplicate directive %q", i, key)
		}
		names[key] = true
	}
	return nil
}

func directiveKey(name, text string) string {
	if strings.TrimSpace(name) != "" {
		return oneLine(name)
	}
	return oneLine(text)
}

// ImportResult reports what an import did (or, dry, would do).
type ImportResult struct {
	BankID               string   `json:"bank_id"`
	BankCreated          bool     `json:"bank_created"`
	ConfigApplied        []string `json:"config_applied"`
	MentalModelsCreated  []string `json:"mental_models_created"`
	MentalModelsUpdated  []string `json:"mental_models_updated"`
	DirectivesCreated    []string `json:"directives_created"`
	DirectivesUpdated    []string `json:"directives_updated"`
	OperationIDs         []string `json:"operation_ids"`
	DryRun               bool     `json:"dry_run"`
	ModelRefreshesQueued int      `json:"mental_model_refreshes_queued"`
}

// ImportTemplate applies a manifest to a bank, creating the bank if needed.
func (e *Engine) ImportTemplate(bankID string, m Manifest, dryRun bool) (*ImportResult, error) {
	if !ValidID(bankID) {
		return nil, invalid("invalid bank id")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	res := &ImportResult{BankID: bankID, DryRun: dryRun, ConfigApplied: []string{}, MentalModelsCreated: []string{},
		MentalModelsUpdated: []string{}, DirectivesCreated: []string{}, DirectivesUpdated: []string{}, OperationIDs: []string{}}
	prof, err := e.Profile(bankID)
	if errors.Is(err, ErrNotFound) {
		res.BankCreated = true
		prof = NewProfile(bankID)
	} else if err != nil {
		return nil, err
	}
	edit := func(p *Profile) error {
		if b := m.Bank; b != nil {
			if b.Name != "" {
				p.Name = b.Name
			}
			if b.Mission != "" {
				p.Mission = b.Mission
			}
			if b.RetainMission != "" {
				p.RetainMission = b.RetainMission
			}
			if b.Disposition != nil {
				p.Disposition = *b.Disposition
			}
			if len(b.Tags) > 0 {
				p.Tags = unionTags(p.Tags, b.Tags)
			}
			keys := make([]string, 0, len(b.Config))
			for k := range b.Config {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if p.Config[k] != b.Config[k] {
					res.ConfigApplied = append(res.ConfigApplied, k)
				}
				p.Config[k] = b.Config[k]
			}
		}
		for _, td := range m.Directives {
			key := strings.ToLower(directiveKey(td.Name, td.Text))
			found := false
			for i := range p.Directives {
				if strings.ToLower(directiveKey(p.Directives[i].Name, p.Directives[i].Text)) == key {
					d := &p.Directives[i]
					d.Name, d.Text, d.Tags, d.Priority, d.Inactive = oneLine(td.Name), oneLine(td.Text), td.Tags, td.Priority, td.Inactive
					res.DirectivesUpdated = append(res.DirectivesUpdated, key)
					found = true
					break
				}
			}
			if !found {
				p.Directives = append(p.Directives, Directive{ID: "d" + shortHash(bankID+"\x00"+key, 10),
					Name: oneLine(td.Name), Text: oneLine(td.Text), Tags: td.Tags, Priority: td.Priority, Inactive: td.Inactive})
				res.DirectivesCreated = append(res.DirectivesCreated, key)
			}
		}
		return nil
	}
	if dryRun {
		cp := *prof
		cp.Config = map[string]string{}
		for k, v := range prof.Config {
			cp.Config[k] = v
		}
		cp.Directives = append([]Directive{}, prof.Directives...)
		if err := edit(&cp); err != nil {
			return nil, err
		}
		if err := validateProfile(&cp); err != nil {
			return nil, err
		}
	} else {
		if res.BankCreated {
			if err := e.CreateBank(prof); err != nil && !errors.Is(err, ErrExists) {
				return nil, err
			}
		}
		if _, err := e.UpdateProfile(bankID, edit); err != nil {
			return nil, err
		}
	}
	for _, tm := range m.MentalModels {
		spec := ModelSpec{ID: tm.ID, Name: strPtr(tm.Name), Question: strPtr(tm.Question)}
		if tm.Tags != nil {
			spec.Tags = &tm.Tags
		}
		if tm.Refresh != "" {
			spec.Refresh = strPtr(tm.Refresh)
		}
		if tm.MaxTokens > 0 {
			spec.MaxTokens = &tm.MaxTokens
		}
		if tm.Budget != "" {
			spec.Budget = strPtr(tm.Budget)
		}
		if tm.FactTypes != nil {
			spec.FactTypes = &tm.FactTypes
		}
		exists := false
		if !res.BankCreated {
			_, err := e.modelPathByID(bankID, tm.ID)
			exists = err == nil
		}
		if dryRun {
			if exists {
				res.MentalModelsUpdated = append(res.MentalModelsUpdated, tm.ID)
			} else {
				res.MentalModelsCreated = append(res.MentalModelsCreated, tm.ID)
			}
			continue
		}
		if exists {
			// The question and settings only: an import never touches an
			// answer, and never moves a model the person filed elsewhere.
			if _, err := e.UpdateModel(bankID, tm.ID, ModelSpec{Name: spec.Name, Question: spec.Question, Tags: spec.Tags,
				Refresh: spec.Refresh, MaxTokens: spec.MaxTokens, Budget: spec.Budget, FactTypes: spec.FactTypes}); err != nil {
				return nil, err
			}
			res.MentalModelsUpdated = append(res.MentalModelsUpdated, tm.ID)
		} else {
			if spec.Name == nil || *spec.Name == "" {
				spec.Name = strPtr(tm.ID)
			}
			if _, err := e.CreateModel(bankID, spec); err != nil {
				return nil, err
			}
			res.MentalModelsCreated = append(res.MentalModelsCreated, tm.ID)
		}
		if e.AI.Available() {
			if op, _, err := e.EnqueueRefresh(bankID, tm.ID); err == nil {
				res.OperationIDs = append(res.OperationIDs, op)
				res.ModelRefreshesQueued++
			}
		}
	}
	return res, nil
}

func strPtr(s string) *string { return &s }

// ExportTemplate renders a bank's configuration as a manifest: its profile
// fields as set, its settings, its models' questions and its directives.
func (e *Engine) ExportTemplate(bankID string) (*Manifest, error) {
	p, err := e.Profile(bankID)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Version: "1", Bank: &TemplateBank{Name: p.Name, Mission: p.Mission, RetainMission: p.RetainMission,
		Tags: p.Tags, Config: p.Config}}
	if p.Disposition != (Disposition{3, 3, 3}) {
		d := p.Disposition
		m.Bank.Disposition = &d
	}
	if len(m.Bank.Config) == 0 {
		m.Bank.Config = nil
	}
	if len(m.Bank.Tags) == 0 {
		m.Bank.Tags = nil
	}
	models, err := e.ListModels(bankID, ModelQuery{})
	if err != nil {
		return nil, err
	}
	for _, mm := range models {
		tm := TemplateModel{ID: mm.ID, Name: mm.Name, Question: mm.Question, Refresh: mm.Refresh, MaxTokens: mm.MaxTokens,
			Budget: mm.Budget, FactTypes: mm.FactTypes}
		if len(mm.Tags) > 0 {
			tm.Tags = mm.Tags
		}
		m.MentalModels = append(m.MentalModels, tm)
	}
	for _, d := range p.Directives {
		m.Directives = append(m.Directives, TemplateDirective{Name: d.Name, Text: d.Text, Tags: d.Tags,
			Priority: d.Priority, Inactive: d.Inactive})
	}
	return m, nil
}
