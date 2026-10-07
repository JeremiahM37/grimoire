// Package agenthook carries the coding-agent hook script inside the binary, so
// `grimoire agent install` can put it where an agent will run it without the
// source tree being present.
//
// clients/hooks/grimoire_bank_session.py is the file people read and test; the
// copy here is what ships. TestEmbeddedHookMatchesTheClientCopy keeps the two
// identical.
package agenthook

import _ "embed"

// Script is the hook, a Python 3 standard-library program.
//
//go:embed grimoire_bank_session.py
var Script []byte

// FileName is what the installer writes it as, and how it recognises its own
// hook entries in an agent's settings.
const FileName = "grimoire_bank_session.py"
