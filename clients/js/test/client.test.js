// Tests for the JS client.
//
// They run against a stub server rather than a real Grimoire: what is under
// test is the client's half of the contract — method, path, query string,
// request body, and the shapes it hands back — and a real server would make
// those assertions harder to see, not easier. The server's half is tested in
// go/internal/api.

import assert from 'node:assert/strict'
import { after, beforeEach, describe, it } from 'node:test'
import { createServer } from 'node:http'

import Grimoire, { GrimoireError, NotFound, Unauthorized, VaultLocked, stored, superseded } from '../index.js'

const calls = []
let reply = {}
let status = 200

const server = createServer((req, res) => {
  const chunks = []
  req.on('data', (c) => chunks.push(c))
  req.on('end', () => {
    const raw = Buffer.concat(chunks).toString()
    calls.push({
      method: req.method,
      path: req.url,
      body: raw ? JSON.parse(raw) : undefined,
      headers: req.headers,
    })
    const payload = JSON.stringify(reply)
    res.writeHead(status, { 'Content-Type': 'application/json' })
    res.end(payload)
  })
})
await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
after(() => server.close())

const last = () => {
  assert.ok(calls.length, 'no request was made')
  return calls[calls.length - 1]
}

let client
beforeEach(() => {
  calls.length = 0
  reply = {}
  status = 200
  client = new Grimoire(base, { token: 'test-token', agent: 'node-agent' })
})

describe('memory', () => {
  it('posts a fact with its agent', async () => {
    reply = { op: 'ADD', id: 'a1', results: [{ op: 'ADD', id: 'a1' }] }
    const result = await client.add('the user prefers tabs', { topic: 'prefs' })

    assert.equal(last().method, 'POST')
    assert.equal(last().path, '/api/memory')
    assert.deepEqual(last().body, {
      text: 'the user prefers tabs', agent: 'node-agent', topic: 'prefs',
    })
    assert.equal(result.op, 'ADD')
    assert.equal(stored(result), true)
  })

  it('sends only the scope fields it was given', async () => {
    reply = { op: 'ADD' }
    await client.add('x')
    assert.deepEqual(Object.keys(last().body).sort(), ['agent', 'text'])
  })

  it('forwards scope, lifetime and pinning', async () => {
    reply = { op: 'ADD' }
    await client.add('x', {
      session: 'run-9', category: 'gotcha', expires_in: '72h', immutable: true,
    })
    assert.equal(last().body.session, 'run-9')
    assert.equal(last().body.category, 'gotcha')
    assert.equal(last().body.expires_in, '72h')
    assert.equal(last().body.immutable, true)
  })

  it('sends infer=false but leaves the default unstated', async () => {
    reply = { op: 'ADD' }
    await client.add('x', { infer: false })
    assert.equal(last().body.infer, false)
    await client.add('x')
    assert.equal('infer' in last().body, false)
  })

  it('reads the authoritative results list', async () => {
    // The top level repeats only part of the first result; parsing the list is
    // what keeps `target` and `why` from going missing.
    reply = {
      op: 'UPDATE', id: 'new',
      results: [{ op: 'UPDATE', id: 'new', target: 'old', why: 'supersedes: ...' }],
    }
    const result = await client.add('the user prefers tabs')
    assert.equal(result.target, 'old')
    assert.equal(result.why, 'supersedes: ...')
  })

  it('reports a no-op as not stored', async () => {
    reply = { op: 'NOOP', results: [{ op: 'NOOP', target: 'old' }] }
    assert.equal(stored(await client.add('x')), false)
  })

  it('builds the search query string', async () => {
    reply = []
    await client.search('indentation', {
      limit: 5, agent: 'claude', session: 'run-1', category: 'preference',
      includeSuperseded: true, explain: true,
    })
    const { path } = last()
    for (const fragment of [
      'q=indentation', 'limit=5', 'agent=claude', 'session=run-1',
      'category=preference', 'include_superseded=1', 'explain=1',
    ]) {
      assert.ok(path.includes(fragment), `${path} missing ${fragment}`)
    }
  })

  it('omits flags that are off', async () => {
    reply = []
    await client.search('x')
    assert.ok(!last().path.includes('include_superseded'))
    assert.ok(!last().path.includes('explain'))
  })

  it('hands back facts as the server sent them', async () => {
    reply = [{ id: 'a1', text: 'the user prefers tabs', path: 'memory/prefs.md', score: 0.8 }]
    const [fact] = await client.search('indentation')
    assert.equal(fact.text, 'the user prefers tabs')
    assert.equal(superseded(fact), false)
  })

  it('marks a superseded fact', async () => {
    reply = [{ id: 'a1', text: 'old belief', superseded_by: 'b2' }]
    const [fact] = await client.search('x', { includeSuperseded: true })
    assert.equal(superseded(fact), true)
  })

  it('asks for everything in scope with getAll', async () => {
    reply = []
    await client.getAll({ agent: 'claude' })
    assert.ok(last().path.includes('limit=200'))
    assert.ok(!last().path.includes('q='))
  })

  it('asks as of an instant with history', async () => {
    reply = []
    await client.history('2026-08-14T09:00:00Z')
    assert.ok(last().path.includes('as_of=2026-08-14T09%3A00%3A00Z'), last().path)
  })

  it('defaults each batch item agent without overriding an explicit one', async () => {
    reply = { results: [{ op: 'ADD' }, { op: 'ADD' }] }
    const results = await client.addMany([
      { text: 'one' },
      { text: 'two', agent: 'other-agent' },
    ])
    assert.equal(last().body.items[0].agent, 'node-agent')
    assert.equal(last().body.items[1].agent, 'other-agent')
    assert.equal(results.length, 2)
  })

  it('does not mutate the caller\'s batch items', async () => {
    reply = { results: [] }
    const original = { text: 'one' }
    await client.addMany([original])
    assert.deepEqual(original, { text: 'one' })
  })

  it('patches only the fields given', async () => {
    reply = {}
    await client.update('memory/prefs.md', 'a1', { text: 'new wording' })
    assert.equal(last().method, 'PATCH')
    assert.deepEqual(last().body, { path: 'memory/prefs.md', id: 'a1', text: 'new wording' })
  })

  it('can clear a pin', async () => {
    // false is a value, not an absence.
    reply = {}
    await client.update('memory/prefs.md', 'a1', { immutable: false })
    assert.equal(last().body.immutable, false)
  })

  it('retracts softly by default and hard on request', async () => {
    reply = {}
    await client.delete('memory/prefs.md', 'a1')
    assert.equal(last().method, 'DELETE')
    assert.ok(!last().path.includes('hard'))
    assert.ok(last().path.includes('agent=node-agent'))

    await client.delete('memory/prefs.md', 'a1', { hard: true })
    assert.ok(last().path.includes('hard=1'))
  })

  it('exports with and without filters', async () => {
    reply = { count: 0, entries: [] }
    await client.export()
    assert.equal(last().path, '/api/memory/export')
    await client.export({ agent: 'claude' })
    assert.equal(last().path, '/api/memory/export?agent=claude')
  })

  it('lists scopes', async () => {
    reply = { agents: { claude: 3 } }
    const scopes = await client.scopes()
    assert.equal(last().path, '/api/memory/facets')
    assert.equal(scopes.agents.claude, 3)
  })
})

describe('knowledge', () => {
  it('asks with a citation limit', async () => {
    reply = { answer: '8443' }
    await client.ask('what port', { limit: 3 })
    assert.equal(last().path, '/api/ask')
    assert.deepEqual(last().body, { question: 'what port', k: 3 })
  })

  it('reads and writes notes', async () => {
    reply = { body: '# x' }
    await client.readNote('sub/note.md')
    assert.equal(last().path, '/api/notes/sub/note.md')

    reply = {}
    await client.writeNote('a.md', '# A', { title: 'A' })
    assert.equal(last().method, 'PUT')
    assert.deepEqual(last().body, { body: '# A', frontmatter: { title: 'A' } })
  })

  it('escapes a path without destroying its separators', async () => {
    reply = {}
    await client.readNote('my notes/a b.md')
    assert.ok(last().path.includes('my%20notes/a%20b.md'), last().path)
  })
})

describe('transport', () => {
  it('sends the token as a bearer', async () => {
    reply = {}
    await client.health()
    assert.equal(last().headers.authorization, 'Bearer test-token')
  })

  it('sends no header without a token', async () => {
    reply = {}
    await new Grimoire(base).health()
    assert.equal('authorization' in last().headers, false)
  })

  it('maps status codes to error types', async () => {
    for (const [code, type] of [[404, NotFound], [401, Unauthorized],
      [403, Unauthorized], [423, VaultLocked], [500, GrimoireError]]) {
      status = code
      reply = { error: 'nope' }
      await assert.rejects(() => client.health(), (err) => {
        assert.ok(err instanceof type, `${code} produced ${err.name}`)
        assert.equal(err.status, code)
        assert.equal(err.detail, 'nope')
        return true
      })
    }
  })

  it('reports an unreachable server as a GrimoireError', async () => {
    const offline = new Grimoire('http://127.0.0.1:1')
    await assert.rejects(() => offline.health(), (err) => {
      assert.ok(err instanceof GrimoireError)
      assert.equal(err.status, 0)
      assert.ok(err.detail.includes('cannot reach'))
      return true
    })
  })

  it('does not double up a trailing slash', async () => {
    reply = {}
    await new Grimoire(`${base}/`).health()
    assert.equal(last().path, '/api/health')
  })

  it('accepts an injected fetch', async () => {
    let seen
    const custom = new Grimoire(base, {
      fetch: async (url, init) => {
        seen = { url, init }
        return new Response('{"ok":true}', { status: 200 })
      },
    })
    assert.deepEqual(await custom.health(), { ok: true })
    assert.ok(seen.url.endsWith('/api/health'))
  })
})

describe('reconciliation scope', () => {
  it('is forwarded when set and unstated by default', async () => {
    reply = { op: 'ADD' }
    await client.add('x', { scope: 'topic' })
    assert.equal(last().body.scope, 'topic')

    await client.add('x')
    assert.equal('scope' in last().body, false)
  })
})

describe('feedback', () => {
  it('posts a verdict on a recalled fact', async () => {
    reply = { helpful: 1, unhelpful: 0, usefulness: 0.66 }
    await client.feedback('memory/ops.md', 'a1', true)
    assert.equal(last().path, '/api/memory/feedback')
    assert.deepEqual(last().body, { path: 'memory/ops.md', id: 'a1', helpful: true })
  })
})

describe('entity graph', () => {
  it('builds its query', async () => {
    reply = { seed: 'priya sharma', nodes: [], edges: [], entries: [] }
    await client.graph('priya', { depth: 2, limit: 10, session: 'run-1' })
    for (const fragment of ['entity=priya', 'depth=2', 'limit=10', 'session=run-1']) {
      assert.ok(last().path.includes(fragment), `${last().path} missing ${fragment}`)
    }
  })

  it('omits the entity for an overview', async () => {
    reply = { seed: '', nodes: [], edges: [], entries: [] }
    await client.graph()
    assert.ok(!last().path.includes('entity='))
  })
})
