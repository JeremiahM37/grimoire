// Runs the plugin's client and line parser against a real Grimoire server.
//
// The scenario is the one the plugin exists for: an agent writes a fact, the
// person fixes that line by editing the file (as they would in Obsidian), the
// agent repeats its old belief, and the disagreement has to surface as a
// challenge the person can settle — with the plugin's own reading of the file
// agreeing with the server about whose line is whose.
//
// Needs a built server: `go -C ../../go build -o grimoire ./cmd/grimoire`, or
// GRIMOIRE_BIN pointing at one.

import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, readFileSync, writeFileSync, existsSync, rmSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { GrimoireClient, type Transport } from '../src/api.ts'
import { authorityOf, parseMemoryLine } from '../src/memoryline.ts'

const BIN = process.env.GRIMOIRE_BIN ?? resolve(import.meta.dirname, '../../../go/grimoire')

let proc: ChildProcess
let vault: string
let base: string
let client: GrimoireClient

const fetchTransport: Transport = async (req) => {
  const res = await fetch(req.url, { method: req.method, headers: req.headers, body: req.body })
  return { status: res.status, text: await res.text() }
}

function freePort(): Promise<number> {
  return new Promise((ok) => {
    const s = createServer()
    s.listen(0, '127.0.0.1', () => {
      const port = (s.address() as { port: number }).port
      s.close(() => ok(port))
    })
  })
}

async function until<T>(what: string, fn: () => Promise<T | undefined>, ms = 15000): Promise<T> {
  const end = Date.now() + ms
  let last: unknown
  while (Date.now() < end) {
    try {
      const v = await fn()
      if (v !== undefined) return v
    } catch (e) {
      last = e
    }
    await new Promise((r) => setTimeout(r, 150))
  }
  throw new Error(`timed out waiting for ${what}${last ? `: ${last}` : ''}`)
}

/** An agent writing through the API, as grimoire-mcp would. */
async function agentRemembers(agent: string, text: string, topic: string) {
  const res = await fetch(`${base}/api/memory`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text, topic, agent }),
  })
  assert.ok(res.status === 200 || res.status === 201, await res.clone().text())
  return (await res.json()) as { results: Array<{ op: string; id: string; path: string; challenges?: string }> }
}

function noteLines(path: string): string[] {
  return readFileSync(join(vault, path), 'utf8').split('\n')
}

before(async () => {
  assert.ok(existsSync(BIN), `no server binary at ${BIN} — build it first`)
  vault = mkdtempSync(join(tmpdir(), 'grimoire-obsidian-it-'))
  const port = await freePort()
  base = `http://127.0.0.1:${port}`
  const env: NodeJS.ProcessEnv = { ...process.env, GRIMOIRE_VAULT: vault, GRIMOIRE_PORT: String(port) }
  delete env.GRIMOIRE_SESSION
  delete env.GRIMOIRE_URL
  proc = spawn(BIN, [], { env, stdio: ['ignore', 'ignore', 'pipe'] })
  let stderr = ''
  proc.stderr!.on('data', (d) => (stderr += d))
  client = new GrimoireClient(base, '', fetchTransport)
  await until('server health', async () => ((await client.health()) ? true : undefined)).catch((e) => {
    throw new Error(`${e.message}\n${stderr}`)
  })
})

after(() => {
  proc?.kill()
  if (vault) rmSync(vault, { recursive: true, force: true })
})

test('a hand edit in the file outranks the agent, and the plugin agrees', async () => {
  const first = await agentRemembers('codex', 'Billing Postgres runs on port 5432', 'ops')
  const path = first.results[0].path
  assert.equal(path, 'memory/ops.md')

  // The plugin reads the agent's line as the agent's.
  const lines = noteLines(path)
  const idx = lines.findIndex((l) => l.includes('port 5432'))
  const agentLine = parseMemoryLine(lines[idx])!
  assert.equal(agentLine.agent, 'codex')
  assert.equal(authorityOf(agentLine), 'agent')

  // The person fixes it in their editor: change the text, leave the trailer.
  lines[idx] = lines[idx].replace('port 5432', 'port 6432')
  writeFileSync(join(vault, path), lines.join('\n'))
  const fixed = parseMemoryLine(lines[idx])!
  assert.equal(fixed.handWritten, true)
  assert.equal(authorityOf(fixed), 'human', 'the plugin must show the edited line as yours')

  // The server reaches the same verdict once its watcher sees the edit.
  await until('server to index the hand edit', async () => {
    const res = await fetch(`${base}/api/memory?q=billing+postgres+port&limit=5`)
    const hits = (await res.json()) as Array<{ text: string; authority: string }>
    return hits.find((h) => h.text.includes('6432') && h.authority === 'human') ? true : undefined
  })

  // The agent repeats its old belief. It may not overwrite the person's line.
  const again = await agentRemembers('codex', 'Billing Postgres runs on port 5432', 'ops')
  assert.equal(again.results[0].op, 'ADD', 'the agent must not supersede a person')
  assert.ok(again.results[0].challenges, 'the refusal has to be recorded as a challenge')

  const open = await client.challenges()
  assert.equal(open.length, 1)
  assert.equal(open[0].agent, 'codex')
  assert.equal(open[0].contested_text, 'Billing Postgres runs on port 6432')
  assert.equal(open[0].contested_authority, 'human')
  assert.equal(open[0].note, path)

  // In the file, the plugin can find the disputing line and what it disputes.
  const now = noteLines(path).map(parseMemoryLine).filter((e) => e !== null)
  const disputing = now.find((e) => e!.id === open[0].id)!
  assert.equal(disputing.challenges, open[0].contested_id)
  assert.ok(now.some((e) => e!.id === open[0].contested_id && authorityOf(e!) === 'human'))

  // "Keep mine": the agent's claim is struck, the person's stands.
  await client.resolveChallenge(open[0].note, open[0].id, 'uphold')
  assert.deepEqual(await client.challenges(), [])
  const settled = noteLines(path).map(parseMemoryLine).filter((e) => e !== null)
  assert.equal(settled.find((e) => e!.id === open[0].id)!.struck, true)
  assert.equal(settled.find((e) => e!.text.includes('6432'))!.struck, false)
})

test('a fact told from Obsidian is a person\'s fact, and conceding hands it over', async () => {
  const told = await client.rememberAsHuman('The staging deploy window is 14:00 UTC', 'release', 'jeremiah')
  assert.ok(told, 'remember returned nothing')
  const lines = noteLines('memory/release.md')
  const mine = parseMemoryLine(lines.find((l) => l.includes('14:00 UTC'))!)!
  assert.equal(mine.human, true)
  assert.equal(mine.agent, 'jeremiah')
  assert.equal(authorityOf(mine), 'human')

  const contra = await agentRemembers('claude-code', 'The staging deploy window is 16:00 UTC', 'release')
  assert.equal(contra.results[0].op, 'ADD')
  assert.ok(contra.results[0].challenges)

  const open = (await client.challenges()).filter((c) => c.note === 'memory/release.md')
  assert.equal(open.length, 1)
  // "Agent is right": the agent's fact supersedes the person's after all.
  await client.resolveChallenge(open[0].note, open[0].id, 'concede')
  assert.deepEqual(await client.challenges(), [])
  const after = noteLines('memory/release.md').map(parseMemoryLine).filter((e) => e !== null)
  assert.equal(after.find((e) => e!.text.includes('14:00'))!.struck, true)
  assert.equal(after.find((e) => e!.text.includes('16:00'))!.struck, false)
})

test('activity, agents and retrieval read the same server', async () => {
  const changes = await client.changes('1d')
  assert.ok(changes.changes.some((c) => c.agent === 'codex' && c.path === 'memory/ops.md'))

  const agents = await client.agents()
  const codex = agents.find((a) => a.agent === 'codex')
  assert.ok(codex && codex.facts >= 1, JSON.stringify(agents))

  await fetch(`${base}/api/notes`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path: 'runbooks/kestrel.md', body: '# Kestrel\n\nThe kestrel gateway uses copper certificates.' }),
  })
  const chunks = await until('retrieval to index the note', async () => {
    const r = await client.retrieve('kestrel gateway certificates', 3)
    return r.some((c) => c.path === 'runbooks/kestrel.md') ? r : undefined
  })
  assert.match(chunks.find((c) => c.path === 'runbooks/kestrel.md')!.chunk, /copper certificates/)
})

test('errors carry the server\'s message', async () => {
  await assert.rejects(
    client.resolveChallenge('memory/ops.md', 'nope', 'sideways' as 'uphold'),
    /resolution must be/,
  )
  const wrong = new GrimoireClient(base.replace(/:\d+$/, ':1'), '', fetchTransport)
  await assert.rejects(wrong.health())
})
