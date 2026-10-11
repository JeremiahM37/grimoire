package seal

import (
	"bytes"
	"encoding/json"
	"strings"
)

// envelopePrefix marks a sealed plaintext that carries the fact's evidence and
// check together with its text. The leading NUL keeps an ordinary fact from
// starting with it by accident.
const envelopePrefix = "\x00seal/v1\n"

// Payload is everything a sealed fact keeps out of the note file. Evidence and
// Check were once written in clear on the bullet; they now travel inside the
// ciphertext with the text, so a reader who cannot open the fact cannot read
// what it cites or how it is checked.
type Payload struct {
	Text     string `json:"text"`
	Evidence string `json:"evidence,omitempty"`
	Check    string `json:"check,omitempty"`
}

// EncodeSealed is the plaintext a sealed fact is encrypted from.
func EncodeSealed(p Payload) string {
	raw, err := json.Marshal(p)
	if err != nil {
		// A struct of strings cannot fail to marshal.
		panic("seal: encoding payload: " + err.Error())
	}
	return envelopePrefix + string(raw)
}

// DecodeSealed reads a decrypted sealed plaintext. It reports whether the
// plaintext was an envelope. A plaintext that is not one is the bare text a
// fact was sealed as before envelopes existed; it comes back as Payload.Text
// with nothing else, and the caller decides where its metadata lives.
//
// A bare text that happens to start with the prefix but is not a well-formed
// envelope is treated as bare text, so no legacy fact can be misread as an
// envelope it is not.
func DecodeSealed(plain string) (Payload, bool) {
	if !strings.HasPrefix(plain, envelopePrefix) {
		return Payload{Text: plain}, false
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(plain[len(envelopePrefix):])))
	dec.DisallowUnknownFields()
	var p Payload
	if err := dec.Decode(&p); err != nil || dec.More() {
		return Payload{Text: plain}, false
	}
	return p, true
}
