package secrets

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"sync"
)

// Redacting the values this vault holds from text it did not write.
//
// RedactText catches credentials by shape. It cannot catch a password, a
// homelab API token with no issuer prefix, or anything else that looks like
// ordinary text — yet those are exactly what a note or a memory picks up when
// someone pastes a working command into it. The vault knows those values, so
// text leaving the server for an agent (the hook's injected memory, the
// session's standing memory) is checked against them as well.
//
// What is kept is a set of keyed fingerprints, not the values: an HMAC under a
// key made fresh for this process, of every current and past value. It is
// rebuilt whenever the payload is decrypted or written, and it survives Lock,
// so text served while the vault is locked is still checked against what the
// vault last held. A restart starts with an empty set until the first unlock.

// knownMinLen is the shortest value worth matching. Shorter values (a port, a
// username, "admin") would turn ordinary words into redactions.
const knownMinLen = 8

type knownSet struct {
	mu   sync.RWMutex
	key  []byte
	mark map[string]string // fingerprint -> secret name
	lens map[int]bool      // value lengths present, to skip most tokens cheaply
}

func (k *knownSet) fingerprint(value string) string {
	m := hmac.New(sha256.New, k.key)
	m.Write([]byte(value))
	return string(m.Sum(nil))
}

// rebuild replaces the set from a decrypted payload.
func (k *knownSet) rebuild(payload map[string]secretEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key == nil {
		k.key = make([]byte, 32)
		if _, err := rand.Read(k.key); err != nil {
			k.key = nil
			return
		}
	}
	k.mark, k.lens = map[string]string{}, map[int]bool{}
	add := func(name, value string) {
		value = strings.TrimSpace(value)
		if len(value) < knownMinLen {
			return
		}
		k.mark[k.fingerprint(value)] = name
		k.lens[len(value)] = true
	}
	for name, entry := range payload {
		if configValue(name, entry) {
			continue
		}
		add(name, entry.Value)
		for _, old := range entry.Versions {
			add(name, old.Value)
		}
	}
}

// configValue is a value stored beside credentials rather than one: a host,
// a user name, a URL imported from a secret file. Redacting those turned a
// machine's name into "[vault secret files/…/host]" everywhere it appeared in
// a note, which hid what the note was about and protected nothing. Values
// imported since secret files learned the difference say so in their note;
// older imports are judged by their key, the same way the import does.
func configValue(name string, entry secretEntry) bool {
	if note, _ := entry.Meta[MetaNote].(string); strings.HasPrefix(note, ConfigNotePrefix) {
		return true
	}
	return strings.HasPrefix(name, "files/") && !CredentialKey(name)
}

// tokenBreak ends a candidate token: whitespace, quotes, brackets and the
// punctuation that sits around a value in a command or a config line.
func tokenBreak(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', '<', '>', '(', ')', '[', ']', '{', '}', ',', ';', '|':
		return true
	}
	return false
}

// pieces are the spans inside a token a value can occupy on its own: the
// token, and the parts around a separator such as KEY=value or
// user:pass@host.
func pieces(token string) []string {
	out := []string{token}
	for _, sep := range []string{"=", ":", "@", "/"} {
		for _, part := range strings.Split(token, sep) {
			if part != token && part != "" {
				out = append(out, part)
			}
		}
	}
	// user:pass@host needs every separator at once.
	return append(out, strings.FieldsFunc(token, func(r rune) bool { return strings.ContainsRune("=:@/", r) })...)
}

// RedactKnown replaces every value this vault holds, current or past, with a
// marker naming the secret, and reports how many it replaced.
func (v *Vault) RedactKnown(text string) (string, int) {
	k := &v.known
	k.mu.RLock()
	defer k.mu.RUnlock()
	if len(k.mark) == 0 {
		return text, 0
	}
	count := 0
	var b strings.Builder
	start := -1
	flush := func(end int) {
		token := text[start:end]
		replaced := token
		done := map[string]bool{}
		for _, piece := range pieces(strings.TrimRight(token, ".")) {
			if done[piece] || !k.lens[len(piece)] {
				continue
			}
			done[piece] = true
			if name, ok := k.mark[k.fingerprint(piece)]; ok {
				replaced = strings.Replace(replaced, piece, "[vault secret "+name+"]", 1)
				count++
			}
		}
		b.WriteString(replaced)
	}
	for i, r := range text {
		if tokenBreak(r) {
			if start >= 0 {
				flush(i)
				start = -1
			}
			b.WriteRune(r)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		flush(len(text))
	}
	return b.String(), count
}
