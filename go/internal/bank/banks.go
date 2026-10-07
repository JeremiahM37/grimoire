package bank

import (
	"errors"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Profile reads a bank's bank.md.
func (e *Engine) Profile(id string) (*Profile, error) {
	if !ValidID(id) {
		return nil, invalid("invalid bank id")
	}
	n, err := e.Vault.Read(ProfilePath(id))
	if err != nil {
		if errors.Is(err, vault.ErrVault) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return ParseProfile(id, n.Frontmatter, n.Body), nil
}

// writeProfile writes bank.md and indexes it.
func (e *Engine) writeProfile(p *Profile, snapshot bool) error {
	rel := ProfilePath(p.ID)
	if snapshot && e.History != nil {
		if n, err := e.Vault.Read(rel); err == nil {
			e.History.Snapshot(rel, n.Body)
		}
	}
	if _, err := e.Vault.Write(rel, p.Body(), e.keepFrontmatter(rel, p.Frontmatter())); err != nil {
		return err
	}
	_, err := e.Index.Upsert(rel)
	return err
}

// CreateBank creates a bank. It fails with ErrExists when bank.md is there.
func (e *Engine) CreateBank(p *Profile) error {
	if !ValidID(p.ID) {
		return invalid("bank id must match [a-z0-9][a-z0-9._:-]{0,63}")
	}
	if err := validateProfile(p); err != nil {
		return err
	}
	lock := e.bankLock(p.ID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := e.Profile(p.ID); err == nil {
		return ErrExists
	}
	return e.writeProfile(p, false)
}

func validateProfile(p *Profile) error {
	if err := ValidateDisposition(p.Disposition); err != nil {
		return invalid("%v", err)
	}
	if err := ValidateConfig(p.Config); err != nil {
		return invalid("%v", err)
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = p.ID
	}
	for i := range p.Directives {
		p.Directives[i].Text = oneLine(p.Directives[i].Text)
		if p.Directives[i].Text == "" {
			return invalid("directive %d is empty", i)
		}
		if p.Directives[i].ID == "" {
			p.Directives[i].ID = "d" + shortHash(p.Directives[i].Text, 10)
		}
	}
	return nil
}

// UpdateProfile applies edit to a bank's profile and writes it. Only an
// explicit call reaches here: nothing a model produces edits a profile.
func (e *Engine) UpdateProfile(id string, edit func(*Profile) error) (*Profile, error) {
	lock := e.bankLock(id)
	lock.Lock()
	defer lock.Unlock()
	p, err := e.Profile(id)
	if err != nil {
		return nil, err
	}
	if err := edit(p); err != nil {
		return nil, err
	}
	p.ID = id
	if err := validateProfile(p); err != nil {
		return nil, err
	}
	if err := e.writeProfile(p, true); err != nil {
		return nil, err
	}
	return e.Profile(id)
}

// DeleteBank removes every file of a bank.
func (e *Engine) DeleteBank(id string) error {
	lock := e.bankLock(id)
	lock.Lock()
	defer lock.Unlock()
	if _, err := e.Profile(id); err != nil {
		return err
	}
	rels, err := e.Vault.WalkDir(strings.TrimSuffix(Prefix(id), "/"))
	if err != nil {
		return err
	}
	for _, rel := range rels {
		if e.History != nil {
			if n, err := e.Vault.Read(rel); err == nil {
				e.History.Snapshot(rel, n.Body)
			}
		}
		if err := e.Vault.Delete(rel); err != nil {
			return err
		}
		if err := e.Index.Remove(rel); err != nil {
			return err
		}
	}
	e.bump(id)
	return nil
}

// BankSummary is one row of the bank list.
type BankSummary struct {
	ID        string `json:"bank_id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Facts     int    `json:"facts"`
	Documents int    `json:"documents"`
	Updated   string `json:"updated,omitempty"`
}

// ListBanks lists the banks the index knows, with counts.
func (e *Engine) ListBanks() ([]BankSummary, error) {
	rows, err := e.Index.DB.Query("SELECT id, path, name, updated FROM bank_banks ORDER BY id")
	if err != nil {
		return nil, err
	}
	var out []BankSummary
	for rows.Next() {
		var b BankSummary
		if err := rows.Scan(&b.ID, &b.Path, &b.Name, &b.Updated); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, b)
	}
	rows.Close()
	counts := func(q string) map[string]int {
		m := map[string]int{}
		r, err := e.Index.DB.Query(q)
		if err != nil {
			return m
		}
		defer r.Close()
		for r.Next() {
			var id string
			var n int
			if r.Scan(&id, &n) == nil {
				m[id] = n
			}
		}
		return m
	}
	facts := counts("SELECT bank, COUNT(*) FROM bank_units GROUP BY bank")
	docs := counts("SELECT bank, COUNT(*) FROM bank_documents GROUP BY bank")
	for i := range out {
		out[i].Facts, out[i].Documents = facts[out[i].ID], docs[out[i].ID]
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}
