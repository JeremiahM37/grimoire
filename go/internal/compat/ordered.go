package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// OrderedObject is a JSON object that remembers key order.
//
// Frontmatter key order is observable — it decides the order keys are written
// back to a note — but JSON objects are unordered by definition, so a fixture
// round-tripped through map[string]any loses exactly the property under test.
// This decodes from the token stream instead, keeping the order the fixture
// file actually has.
type OrderedObject struct {
	Keys   []string
	Values map[string]json.RawMessage
}

func (o *OrderedObject) UnmarshalJSON(data []byte) error {
	o.Keys = nil
	o.Values = map[string]json.RawMessage{}

	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected a JSON object, got %v", tok)
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("expected an object key, got %v", keyTok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if _, seen := o.Values[key]; !seen {
			o.Keys = append(o.Keys, key)
		}
		o.Values[key] = raw
	}
	_, err = dec.Token() // closing brace
	return err
}

// Decode unmarshals the value stored at key into v.
func (o *OrderedObject) Decode(key string, v any) error {
	raw, ok := o.Values[key]
	if !ok {
		return fmt.Errorf("no such key: %q", key)
	}
	return json.Unmarshal(raw, v)
}
