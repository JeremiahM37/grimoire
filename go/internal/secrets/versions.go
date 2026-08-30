package secrets

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Version history, expiry and use tracking for stored secrets.
//
// Put used to overwrite, which made rotation a one-way door: paste the new key,
// discover the service was not ready for it, and the old one is gone. That is
// the single operation a person most needs to undo, and it was the one that
// could not be undone. Every write now keeps the value it replaced.
//
// History lives INSIDE the sealed payload rather than beside it, so an old
// value is protected exactly as well as the current one. A rolled-back
// credential that sat in cleartext next to the vault would be a strange thing
// to have built.

// MaxVersions bounds retained history per secret.
//
// Bounded because the payload is decrypted whole on every unlock: unbounded
// history would make the vault slower for everyone to protect a value almost
// nobody rolls back more than once. Ten is far past the point of usefulness
// while staying small.
const MaxVersions = 10

// Version is a value this secret used to hold.
type Version struct {
	Value string `json:"value"`
	// At is when this value STOPPED being current — the moment it was
	// replaced. Recording the end rather than the start means a version's
	// entry says when it was rotated out, which is the question being asked
	// when somebody is looking at history at all.
	At string `json:"at"`
	// Note is why, when the caller said. Rotation reasons are the difference
	// between history and a list of blobs.
	Note string `json:"note,omitempty"`
}

// Meta keys the vault understands. Anything else a caller puts in Meta is
// carried untouched, so this stays open.
const (
	// MetaExpires is an RFC3339 date after which the credential is dead. The
	// provider knows this and the operator does not, which is why a service
	// dies quietly at renewal time; recording it makes the deadline something
	// the software can warn about.
	MetaExpires = "expires"
	// MetaRotateDays asks for a reminder this many days after the last write,
	// for credentials with no fixed expiry that should still not live forever.
	MetaRotateDays = "rotate_days"
	// MetaCreated and MetaUpdated are maintained by the vault, not the caller.
	MetaCreated = "created"
	MetaUpdated = "updated"
	// MetaLastUsed is when the broker last injected this secret. A credential
	// nobody has used in a year is either dead or a liability, and neither is
	// visible without this.
	MetaLastUsed = "last_used"
	// MetaUses counts brokered uses, so "unused" is distinguishable from
	// "never used".
	MetaUses = "uses"
	// MetaNote is a human description: what this is for, where it came from.
	MetaNote = "note"
)

// Info is everything about a secret except its value.
//
// The type exists so that listing secrets can be rich without any caller
// accidentally serialising a value: there is nowhere in this struct to put one.
type Info struct {
	Name     string `json:"name"`
	Note     string `json:"note,omitempty"`
	Created  string `json:"created,omitempty"`
	Updated  string `json:"updated,omitempty"`
	LastUsed string `json:"last_used,omitempty"`
	Uses     int    `json:"uses"`
	Versions int    `json:"versions"`
	// Expires is the recorded expiry, and ExpiresInDays is negative once past.
	Expires       string `json:"expires,omitempty"`
	ExpiresInDays *int   `json:"expires_in_days,omitempty"`
	// RotateDays and StaleAfterDays describe a reminder rather than a deadline.
	RotateDays int `json:"rotate_days,omitempty"`
	// Status is one of ok, expiring, expired, stale. Computed here so every
	// surface agrees on what "expiring" means rather than each picking a
	// threshold.
	Status string `json:"status"`
}

// Statuses a secret can be in.
const (
	StatusOK       = "ok"
	StatusExpiring = "expiring"
	StatusExpired  = "expired"
	StatusStale    = "stale"
)

// ExpiringSoonDays is how far ahead an expiry starts being reported.
//
// Two weeks because the thing being prevented is a credential dying unnoticed,
// and the recovery is often "ask somebody for a new one" — which takes days,
// not minutes.
const ExpiringSoonDays = 14

// PutVersioned stores a value, keeping the one it replaces.
//
// note explains the change and is recorded against the OLD value, because that
// is the row a person reads when asking "why did this change".
func (v *Vault) PutVersioned(name, value string, meta map[string]any, note string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return err
	}
	now := Now().UTC().Format(time.RFC3339)
	entry, existed := payload[name]

	// Carry forward metadata the caller did not mention. A rotation that
	// supplies only the new value must not silently drop the expiry date and
	// the description somebody set months ago.
	merged := map[string]any{}
	for k, val := range entry.Meta {
		merged[k] = val
	}
	for k, val := range meta {
		if val == nil {
			delete(merged, k)
			continue
		}
		merged[k] = val
	}
	if existed {
		// Only a real change is history. Re-putting an identical value —
		// which a sync or a retried request will do — should not push the
		// previous value out of a bounded list.
		if entry.Value != value {
			entry.Versions = append([]Version{{Value: entry.Value, At: now, Note: note}},
				entry.Versions...)
			if len(entry.Versions) > MaxVersions {
				entry.Versions = entry.Versions[:MaxVersions]
			}
			merged[MetaUpdated] = now
		}
	} else {
		merged[MetaCreated] = now
		merged[MetaUpdated] = now
	}
	entry.Value = value
	entry.Meta = merged
	payload[name] = entry
	return v.writePayloadLocked(payload)
}

// Versions lists the retained history for a secret, newest first, WITHOUT
// values.
//
// Values are omitted deliberately. Listing history is a browsing operation an
// operator does to decide what to restore; handing back every old value to
// answer "how many are there" would defeat the point of not exposing the
// current one.
func (v *Vault) Versions(name string) ([]Version, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return nil, err
	}
	entry, ok := payload[name]
	if !ok {
		return nil, fmt.Errorf("no such secret: %s", name)
	}
	out := make([]Version, 0, len(entry.Versions))
	for _, ver := range entry.Versions {
		out = append(out, Version{At: ver.At, Note: ver.Note})
	}
	return out, nil
}

// Restore makes a previous version current again.
//
// index is 0-based over the list Versions returns. The restore is itself a
// write, so the value being replaced is pushed onto history too — undoing a
// rollback is the same operation as doing one, and nothing is ever destroyed
// by moving backwards.
func (v *Vault) Restore(name string, index int) error {
	v.mu.Lock()
	payload, err := v.payloadLocked()
	if err != nil {
		v.mu.Unlock()
		return err
	}
	entry, ok := payload[name]
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("no such secret: %s", name)
	}
	if index < 0 || index >= len(entry.Versions) {
		v.mu.Unlock()
		return fmt.Errorf("no version %d: %s has %d retained", index, name, len(entry.Versions))
	}
	value := entry.Versions[index].Value
	v.mu.Unlock()
	return v.PutVersioned(name, value, nil,
		fmt.Sprintf("replaced by restore of version %d", index))
}

// MarkUsed records a brokered use.
//
// Best-effort by design: a failure to update the counter must never fail the
// call the counter is counting. The broker is the only caller.
func (v *Vault) MarkUsed(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return
	}
	entry, ok := payload[name]
	if !ok {
		return
	}
	if entry.Meta == nil {
		entry.Meta = map[string]any{}
	}
	entry.Meta[MetaLastUsed] = Now().UTC().Format(time.RFC3339)
	entry.Meta[MetaUses] = asInt(entry.Meta[MetaUses]) + 1
	payload[name] = entry
	_ = v.writePayloadLocked(payload)
}

// Describe returns everything about the secrets except their values.
func (v *Vault) Describe() ([]Info, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(payload))
	for name, entry := range payload {
		out = append(out, describe(name, entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func describe(name string, entry secretEntry) Info {
	m := entry.Meta
	info := Info{
		Name:       name,
		Note:       asString(m[MetaNote]),
		Created:    asString(m[MetaCreated]),
		Updated:    asString(m[MetaUpdated]),
		LastUsed:   asString(m[MetaLastUsed]),
		Uses:       asInt(m[MetaUses]),
		Versions:   len(entry.Versions),
		Expires:    asString(m[MetaExpires]),
		RotateDays: asInt(m[MetaRotateDays]),
		Status:     StatusOK,
	}
	now := Now().UTC()
	if info.Expires != "" {
		if t, err := parseDate(info.Expires); err == nil {
			days := int(t.Sub(now).Hours() / 24)
			info.ExpiresInDays = &days
			switch {
			case days < 0:
				info.Status = StatusExpired
			case days <= ExpiringSoonDays:
				info.Status = StatusExpiring
			}
		}
	}
	// A rotation reminder never overrides a real expiry: a dead credential is
	// a worse fact than an old one, and reporting the milder of the two would
	// hide it.
	if info.Status == StatusOK && info.RotateDays > 0 && info.Updated != "" {
		if t, err := parseDate(info.Updated); err == nil {
			if int(now.Sub(t).Hours()/24) >= info.RotateDays {
				info.Status = StatusStale
			}
		}
	}
	return info
}

// parseDate accepts a full timestamp or a bare date, because an expiry copied
// off a provider's dashboard is usually "2026-11-30" and refusing that would
// make the feature annoying enough to skip.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date %q", s)
}

// NeedsAttention returns the secrets that are expired, expiring or stale.
func (v *Vault) NeedsAttention() ([]Info, error) {
	all, err := v.Describe()
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, i := range all {
		if i.Status != StatusOK {
			out = append(out, i)
		}
	}
	return out, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// asInt reads a number that has been through JSON, where every integer comes
// back as a float64.
func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}
