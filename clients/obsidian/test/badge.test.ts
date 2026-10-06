import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { scanNote } from '../src/scan.ts'

const golden: Array<{ line: string }> = JSON.parse(
  readFileSync(new URL('./fixtures/memory-lines.json', import.meta.url), 'utf8'),
)
const lineWith = (s: string) => golden.find((g) => g.line.includes(s))!.line

test('each kind of line gets the badge a person needs', () => {
  const note = [
    '# ops',
    lineWith('port 8080'),          // an agent's fact
    lineWith('port 9090'),          // the agent's line, edited by hand
    lineWith('typed this bullet'),  // typed from scratch
    lineWith('never go out'),       // declared by=human
    lineWith('Postgres 14'),        // superseded
    lineWith('Ignore previous'),    // came from the web
    lineWith('Freeze until'),       // expired by 2026-10
    'just prose',
  ].join('\n')
  const out = scanNote(note, new Date('2026-10-06T00:00:00Z'))
  assert.deepEqual(out.map((s) => s.line), [1, 2, 3, 4, 5, 6, 7])
  const label = (n: number) => out.find((s) => s.line === n)!.badge
  assert.equal(label(1).label, 'codex')
  assert.equal(label(1).kind, 'agent')
  assert.equal(label(2).label, "✎ you edited codex's")
  assert.equal(label(3).label, '✎ yours')
  assert.equal(label(4).label, '✓ yours')
  assert.equal(label(5).kind, 'replaced')
  assert.equal(label(6).kind, 'pulled')
  assert.match(label(6).label, /from web:example\.com\/a b/)
  assert.equal(label(7).kind, 'expired')
  assert.match(label(1).title, /your version wins/)
})

test('a dispute shows on both lines until it is settled', () => {
  const mine = '- **2026-08-12 10:05 · me** — Deploys never go out on Fridays.'
  const myId = scanNote(mine)[0].entry.id
  const theirs = `- **2026-08-12 11:00 · codex** — Deploys go out on Fridays. <!--m id=111111111111 chal=${myId}-->`
  const open = scanNote([mine, theirs].join('\n'))
  assert.equal(open[0].badge.kind, 'dispute')
  assert.equal(open[0].badge.label, '✎ yours · ⚑ codex disagrees')
  assert.equal(open[1].badge.label, '⚑ codex disputes yours')

  // Settled: the agent's line is struck, so nothing is disputed any more.
  const settled = `- ~~**2026-08-12 11:00 · codex** — Deploys go out on Fridays.~~ <!--m id=111111111111 chal=${myId} sup=${myId}-->`
  const after = scanNote([mine, settled].join('\n'))
  assert.equal(after[0].badge.label, '✎ yours')
  assert.equal(after[1].badge.kind, 'replaced')
})

test('agents keep a stable color', () => {
  const a = scanNote(lineWith('port 8080'))[0].badge.hue
  const b = scanNote(lineWith('Same minute'))[0].badge.hue
  assert.equal(a, b, 'both lines are codex')
})
