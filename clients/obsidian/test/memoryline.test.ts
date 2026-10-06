import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import { authorityOf, deriveID, normalize, parseMemoryLine, vouch } from '../src/memoryline.ts'
import { sha256Hex } from '../src/sha256.ts'

interface Golden {
  line: string
  want: null | Record<string, string | boolean>
}

const golden: Golden[] = JSON.parse(
  readFileSync(new URL('./fixtures/memory-lines.json', import.meta.url), 'utf8'),
)

test('sha256 matches the standard test vectors', () => {
  assert.equal(sha256Hex(''), 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
  assert.equal(sha256Hex('abc'), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad')
  assert.equal(
    sha256Hex('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq'),
    '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1',
  )
  // Multi-block and multi-byte UTF-8.
  assert.equal(sha256Hex('a'.repeat(1000)), '41edece42d63e8d9bf515a9ba6932e1c20cbc9f5a5d134645adb5db1b9737ea3')
  for (const s of ['東京 ½ — naïve', 'x'.repeat(55), 'x'.repeat(56), 'x'.repeat(64), '😀'.repeat(40)]) {
    assert.equal(sha256Hex(s), createHash('sha256').update(s, 'utf8').digest('hex'), s)
  }
})

test('every golden line parses exactly as the Go server parses it', () => {
  assert.ok(golden.length > 10)
  for (const { line, want } of golden) {
    const got = parseMemoryLine(line)
    if (want === null) {
      assert.equal(got, null, `should not be a memory line: ${JSON.stringify(line)}`)
      continue
    }
    assert.ok(got, `should parse: ${JSON.stringify(line)}`)
    for (const key of [
      'id', 'text', 'stamp', 'agent', 'task', 'session', 'category', 'expires', 'immutable',
      'supersededBy', 'supersededAt', 'challenges', 'origin', 'human', 'handWritten',
    ] as const) {
      assert.equal(got[key], want[key], `${key} of ${JSON.stringify(line)}`)
    }
    assert.equal(authorityOf(got), want.authority, `authority of ${JSON.stringify(line)}`)
    assert.equal(normalize(got.text), want.normalized, `normalize of ${JSON.stringify(line)}`)
  }
})

test('the trailer offsets cover exactly the HTML comment', () => {
  const line = golden[0].line + '  '
  const e = parseMemoryLine(line)!
  const trailer = line.slice(e.trailerFrom, e.trailerTo)
  assert.match(trailer, /^ <!--m id=[0-9a-f]{12}-->$/)
  assert.equal(line.slice(0, e.trailerFrom).endsWith('8080.'), true)
})

test('deriveID is the content hash, so editing the text is detectable', () => {
  const id = deriveID('2026-08-14 09:00', 'codex', 'The API listens on port 8080.')
  assert.equal(id, parseMemoryLine(golden[0].line)!.id)
  // Normalisation makes punctuation and case irrelevant to the id.
  assert.equal(id, deriveID('2026-08-14 09:00', 'codex', 'the api LISTENS on port 8080'))
})

test('vouch declares a line as yours without breaking its id', () => {
  const agentLine = golden[0].line
  const before = parseMemoryLine(agentLine)!
  assert.equal(authorityOf(before), 'agent')

  const after = parseMemoryLine(vouch(agentLine))!
  assert.equal(after.human, true)
  assert.equal(after.handWritten, false, 'the id still matches, so this is declared, not inferred')
  assert.equal(after.id, before.id)
  assert.equal(after.text, before.text)
  assert.equal(authorityOf(after), 'human')

  // Idempotent, and a no-op on lines that are already yours or not memory.
  assert.equal(vouch(vouch(agentLine)), vouch(agentLine))
  const typed = '- **2026-08-01 08:00 · me** — I typed this bullet myself.'
  assert.equal(vouch(typed), typed)
  assert.equal(vouch('- plain list item'), '- plain list item')
})

test('vouch keeps a struck line struck and its other fields intact', () => {
  const struck = golden.find((g) => g.line.startsWith('- ~~'))!.line
  const after = parseMemoryLine(vouch(struck))!
  const before = parseMemoryLine(struck)!
  assert.equal(after.struck, true)
  assert.equal(after.supersededBy, before.supersededBy)
  assert.equal(after.supersededAt, before.supersededAt)
  assert.equal(after.human, true)
})
