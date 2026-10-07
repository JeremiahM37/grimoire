package bank

import "strings"

// Directives are a bank's standing rules — "always answer in French", "never
// quote salaries" — kept as bullets in bank.md's "## Directives" section and
// applied to every reflect. Only these explicit calls (and a person editing
// bank.md) change them; no model path reaches them.

// DirectiveSpec creates or patches a directive. Nil is "not sent".
type DirectiveSpec struct {
	Name     *string   `json:"name"`
	Text     *string   `json:"text"`
	Content  *string   `json:"content"` // accepted as a synonym of text
	Tags     *[]string `json:"tags"`
	Priority *int      `json:"priority"`
	Active   *bool     `json:"is_active"`
}

func (s DirectiveSpec) apply(d *Directive) {
	if s.Name != nil {
		d.Name = oneLine(*s.Name)
	}
	if s.Content != nil && s.Text == nil {
		s.Text = s.Content
	}
	if s.Text != nil {
		d.Text = oneLine(*s.Text)
	}
	if s.Tags != nil {
		d.Tags = unionTags(*s.Tags)
	}
	if s.Priority != nil {
		d.Priority = *s.Priority
	}
	if s.Active != nil {
		d.Inactive = !*s.Active
	}
}

// ListDirectives returns a bank's directives, optionally only those whose
// tags reach the given ones.
func (e *Engine) ListDirectives(bankID string, tags []string, activeOnly bool) ([]Directive, error) {
	p, err := e.Profile(bankID)
	if err != nil {
		return nil, err
	}
	out := []Directive{}
	for _, d := range p.Directives {
		if activeOnly && d.Inactive {
			continue
		}
		if len(tags) > 0 && !tagsAllow(d.Tags, tags, "any") {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// CreateDirective adds a directive.
func (e *Engine) CreateDirective(bankID string, spec DirectiveSpec) (*Directive, error) {
	var d Directive
	spec.apply(&d)
	if strings.TrimSpace(d.Text) == "" {
		return nil, invalid("a directive needs text")
	}
	var created Directive
	_, err := e.UpdateProfile(bankID, func(p *Profile) error {
		for _, x := range p.Directives {
			if d.Name != "" && strings.EqualFold(x.Name, d.Name) {
				return invalid("a directive named %q already exists", d.Name)
			}
		}
		d.ID = "d" + shortHash(d.Text+"\x00"+e.now().String(), 10)
		p.Directives = append(p.Directives, d)
		created = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// UpdateDirective patches one directive by id.
func (e *Engine) UpdateDirective(bankID, id string, spec DirectiveSpec) (*Directive, error) {
	var out Directive
	_, err := e.UpdateProfile(bankID, func(p *Profile) error {
		for i := range p.Directives {
			if p.Directives[i].ID == id {
				spec.apply(&p.Directives[i])
				if strings.TrimSpace(p.Directives[i].Text) == "" {
					return invalid("a directive needs text")
				}
				out = p.Directives[i]
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteDirective removes one directive by id.
func (e *Engine) DeleteDirective(bankID, id string) error {
	_, err := e.UpdateProfile(bankID, func(p *Profile) error {
		for i := range p.Directives {
			if p.Directives[i].ID == id {
				p.Directives = append(p.Directives[:i], p.Directives[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
	return err
}
