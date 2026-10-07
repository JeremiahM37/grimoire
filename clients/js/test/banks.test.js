// Tests for the bank client and the memory wrapper, against a fake fetch:
// what is under test is the client's half of the contract.

import assert from 'node:assert/strict'
import { beforeEach, describe, it } from 'node:test'

import Grimoire, { GrimoireError, NotFound } from '../index.js'
import { Bank, Banks, ModelRequired, NotAvailable, formatMemories, withMemory } from '../banks.js'

let calls
let routes
const route = (method, path, body = {}, status = 200) => { routes.set(`${method} ${path}`, { body, status }) }

function fakeFetch(url, init) {
  const u = new URL(url)
  const body = init.body ? JSON.parse(init.body) : undefined
  calls.push({ method: init.method, path: u.pathname, search: u.search, body, headers: init.headers })
  const hit = routes.get(`${init.method} ${u.pathname}`) ?? { body: {}, status: 200 }
  const text = typeof hit.body === 'string' ? hit.body : JSON.stringify(hit.body)
  return Promise.resolve(new Response(text, { status: hit.status }))
}

const find = (method, path) => calls.filter((c) => c.method === method && c.path === path)

let g
let bank
beforeEach(() => {
  calls = []
  routes = new Map()
  g = new Grimoire('http://grimoire.test', { token: 'tok', agent: 'node-agent', fetch: fakeFetch })
  bank = new Bank(g, 'support')
})

describe('bank client', () => {
  it('lists and creates banks with auth and agent headers', async () => {
    route('GET', '/api/banks', { banks: [{ bank_id: 'a' }] })
    const banks = new Banks(g)
    assert.deepEqual(await banks.list(), [{ bank_id: 'a' }])
    await banks.create('b', { mission: 'm' })
    const last = calls.at(-1)
    assert.deepEqual(last.body, { bank_id: 'b', mission: 'm' })
    assert.equal(last.headers.Authorization, 'Bearer tok')
    assert.equal(last.headers['X-Grimoire-Agent'], 'node-agent')
  })

  it('speaks the coding-agent routes', async () => {
    const at = () => calls.at(-1).path + calls.at(-1).search
    await bank.index('cache', { types: ['fact'], limit: 5 })
    assert.equal(at(), '/api/banks/support/index?q=cache&types=fact&limit=5')
    await bank.timeline('#f3a9c1b2', { before: 2 })
    assert.equal(at(), '/api/banks/support/timeline?anchor=%23f3a9c1b2&before=2')
    await bank.getEntries(['#f3a9c1b2', 'o77aa001'])
    assert.equal(at(), '/api/banks/support/lookup?ids=%23f3a9c1b2%2Co77aa001')
    await bank.context({ maxChars: 3000, source: 'resume' })
    assert.equal(at(), '/api/banks/support/context?max_chars=3000&source=resume')
    await bank.writeDigest('a:1', [{ speaker: 'user', text: 'hi' }], { useModel: true })
    assert.equal(calls.at(-1).path, '/api/banks/support/sessions/a%3A1/digest')
    assert.equal(calls.at(-1).body.use_model, true)
  })

  it('escapes the bank id as one segment', async () => {
    await new Bank(g, 'coding-agent:grimoire').profile()
    assert.equal(calls.at(-1).path, '/api/banks/coding-agent%3Agrimoire')
  })

  it('builds a retain item', async () => {
    await bank.retain('Dana moved.', { documentId: 'd1', timestamp: new Date('2024-01-02T00:00:00Z'), tags: ['x'], mode: 'chunks' })
    assert.deepEqual(calls.at(-1).body, {
      items: [{ content: 'Dana moved.', document_id: 'd1', timestamp: '2024-01-02T00:00:00.000Z', tags: ['x'] }],
      mode: 'chunks',
    })
  })

  it('falls back to a synchronous retain when async is refused', async () => {
    let n = 0
    g._fetch = (url, init) => {
      const body = JSON.parse(init.body)
      calls.push({ body })
      n++
      if (body.async) return Promise.resolve(new Response(JSON.stringify({ detail: 'async retain is not available yet; send async=false' }), { status: 400 }))
      return Promise.resolve(new Response(JSON.stringify({ success: true })))
    }
    assert.deepEqual(await bank.retain('x', { async: true }), { success: true, async_fallback: true })
    assert.equal(n, 2)
  })

  it('builds a recall body', async () => {
    route('POST', '/api/banks/support/memories/recall', { results: [] })
    await bank.recall('q', { budget: 'low', maxTokens: 100, types: ['world'], includeEntities: false, includeChunks: 500, trace: true })
    assert.deepEqual(calls.at(-1).body, {
      query: 'q', budget: 'low', max_tokens: 100, types: ['world'],
      include: { entities: null, chunks: { max_tokens: 500 } }, trace: true,
    })
  })

  it('lists memories and force-deletes', async () => {
    await bank.listMemories({ type: 'world', authority: 'human', limit: 5 })
    assert.equal(calls.at(-1).search, '?type=world&authority=human&limit=5')
    await bank.deleteMemory('f1', { force: true })
    assert.equal(calls.at(-1).search, '?force=true')
    await bank.deleteDocument('doc/1')
    assert.equal(calls.at(-1).path, '/api/banks/support/documents/doc%2F1')
  })

  it('tells a missing route from a missing bank', async () => {
    route('POST', '/api/banks/support/reflect', '404 page not found\n', 404)
    await assert.rejects(bank.reflect('why?'), NotAvailable)
    route('POST', '/api/banks/support/reflect', { detail: 'no such bank' }, 404)
    await assert.rejects(bank.reflect('why?'), (e) => e instanceof NotFound && !(e instanceof NotAvailable))
    route('DELETE', '/api/banks/support/operations/o1', 'Method Not Allowed', 405)
    await assert.rejects(bank.cancelOperation('o1'), NotAvailable)
    route('GET', '/api/bank-templates', '404 page not found\n', 404)
    await assert.rejects(new Banks(g).templates(), NotAvailable)
  })

  it('builds the reasoning endpoint paths', async () => {
    route('POST', '/api/banks/support/reflect', { text: 'because' })
    assert.equal((await bank.reflect('why?', { budget: 'low' })).text, 'because')
    assert.deepEqual(calls.at(-1).body, { query: 'why?', budget: 'low', include: { facts: {} } })
    await bank.refreshMentalModel('people/dana')
    assert.equal(calls.at(-1).path, '/api/banks/support/mental-models/people%2Fdana/refresh')
    await bank.rejectProposal('m1')
    assert.equal(calls.at(-1).path, '/api/banks/support/mental-models/m1/proposal/reject')
  })

  it('sends reflect fields under the server names', async () => {
    await bank.reflect('why?', { types: ['world'], tags: ['a'], includeToolCalls: true, excludeMentalModels: true })
    assert.deepEqual(calls.at(-1).body, {
      query: 'why?', fact_types: ['world'], tags: ['a'], exclude_mental_models: true,
      include: { facts: {}, tool_calls: {} },
    })
    await bank.reflect('why?', { includeFacts: false, trace: true })
    assert.deepEqual(calls.at(-1).body, { query: 'why?', trace: true })
  })

  it('creates, edits, moves and exports mental models', async () => {
    route('POST', '/api/banks/support/mental-models', { mental_model: { id: 'people/dana' }, mental_model_id: 'people/dana', operation_id: null }, 201)
    const made = await bank.createMentalModel('Dana', 'Who is Dana?', { folder: 'people', refresh: 'manual', factTypes: ['world'] })
    assert.equal(made.operation_id, null)
    assert.deepEqual(calls.at(-1).body, { name: 'Dana', question: 'Who is Dana?', folder: 'people', refresh: 'manual', fact_types: ['world'] })
    await bank.createMentalModel('Ctx', undefined, { sourceQuery: 'what?', refreshAfterConsolidation: true })
    assert.deepEqual(calls.at(-1).body, { name: 'Ctx', question: 'what?', refresh: 'auto' })
    assert.throws(() => bank.createMentalModel('No question'), TypeError)
    await bank.updateMentalModel('people/dana', { body: 'Mine.' })
    assert.equal(calls.at(-1).method, 'PATCH')
    assert.equal(calls.at(-1).path, '/api/banks/support/mental-models/people%2Fdana')
    await bank.moveMentalModel('people/dana', 'team')
    assert.deepEqual(calls.at(-1).body, { folder: 'team' })
    await bank.mentalModels({ detail: true, folder: 'people' })
    assert.equal(calls.at(-1).search, '?folder=people&detail=full')
    route('GET', '/api/banks/support/mental-models-tree', { roots: [{ kind: 'folder', name: 'people' }] })
    assert.equal((await bank.mentalModelTree())[0].name, 'people')
    route('GET', '/api/banks/support/mental-models-export', '# Index\n')
    assert.equal(await bank.exportMentalModels({ markdown: true }), '# Index\n')
    assert.equal(calls.at(-1).search, '?format=markdown')
  })

  it('throws ModelRequired for a 409 model_required only', async () => {
    route('POST', '/api/banks/support/consolidate', { detail: 'model_required: none configured', code: 'model_required' }, 409)
    await assert.rejects(bank.consolidate(), (e) => e instanceof ModelRequired && e.code === 'model_required' && e.status === 409)
    route('POST', '/api/banks/support/consolidate', { detail: 'something else' }, 409)
    await assert.rejects(bank.consolidate(), (e) => e instanceof GrimoireError && !(e instanceof ModelRequired))
  })

  it('reads observations, directives, operations and templates', async () => {
    route('GET', '/api/banks/support/observations', { items: [{ id: 'o1' }], total: 1, history: [] })
    assert.equal((await bank.observations({ includeHistory: true, authority: 'human' })).items[0].id, 'o1')
    assert.equal(calls.at(-1).search, '?authority=human&include_history=true')
    await bank.deleteObservation('o1', { force: true })
    assert.equal(calls.at(-1).search, '?force=true')
    await bank.directives()
    assert.equal(calls.at(-1).search, '?active_only=false')
    await bank.createDirective('Be brief.', { name: 'Brief', priority: 2 })
    assert.deepEqual(calls.at(-1).body, { text: 'Be brief.', name: 'Brief', priority: 2 })
    await bank.operations({ status: 'queued', type: 'retain' })
    assert.equal(calls.at(-1).search, '?status=queued&type=retain')
    await bank.createWebhook('https://example.com/h', { eventTypes: ['retain.completed'] })
    assert.deepEqual(calls.at(-1).body, { url: 'https://example.com/h', enabled: true, events: ['retain.completed'] })
    await bank.importTemplate('support', { dryRun: true })
    assert.deepEqual(calls.at(-1).body, { template: 'support' })
    assert.equal(calls.at(-1).search, '?dry_run=true')
    await bank.importTemplate({ version: '1', directives: [{ text: 'x' }] })
    assert.deepEqual(calls.at(-1).body, { manifest: { version: '1', directives: [{ text: 'x' }] } })
    await new Banks(g).template('coding-agent')
    assert.equal(calls.at(-1).path, '/api/bank-templates/coding-agent')
  })

  it('waits for an operation to finish', async () => {
    const states = ['queued', 'running', 'completed']
    g._fetch = () => Promise.resolve(new Response(JSON.stringify({ id: 'op-1', kind: 'retain', type: 'retain', status: states.shift() })))
    const op = await bank.waitOperation('op-1', { intervalMs: 0 })
    assert.equal(op.status, 'completed')
    g._fetch = () => Promise.resolve(new Response(JSON.stringify({ id: 'op-1', status: 'running' })))
    await assert.rejects(bank.waitOperation('op-1', { timeoutMs: 0, intervalMs: 0 }), GrimoireError)
  })
})

const FACT = { id: 'f1', text: 'Dana lives in Lyon', type: 'world', occurred_start: '2024-05-01', occurred_end: '2024-05-31' }

function fakeOpenAI(reply = 'Lyon.') {
  const seen = []
  return {
    seen,
    models: { list: () => 'passthrough' },
    chat: {
      completions: {
        async create(params) {
          seen.push(params)
          if (params.stream) {
            return (async function* () { for (const p of ['Ly', 'on']) yield { choices: [{ delta: { content: p } }] } })()
          }
          return { choices: [{ message: { content: reply } }] }
        },
      },
    },
  }
}

describe('withMemory', () => {
  it('injects memories and retains the exchange', async () => {
    route('POST', '/api/banks/support/memories/recall', { results: [FACT] })
    const openai = fakeOpenAI()
    const llm = withMemory(openai, bank, { sessionId: 'chat-7', maxTokens: 512, tags: ['chat'] })
    const messages = [{ role: 'system', content: 'Be brief.' }, { role: 'user', content: 'Where does Dana live?' }]
    await llm.chat.completions.create({ model: 'm1', messages })
    assert.deepEqual(find('POST', '/api/banks/support/memories/recall')[0].body,
      { query: 'Where does Dana live?', budget: 'mid', max_tokens: 512 })
    const sent = openai.seen.at(-1).messages[0].content
    assert.ok(sent.startsWith('Be brief.\n\n# Relevant memories'))
    assert.ok(sent.includes('1. [WORLD] (occurred: 2024-05-01 to 2024-05-31) Dana lives in Lyon'))
    assert.equal(messages[0].content, 'Be brief.')
    const item = find('POST', '/api/banks/support/memories')[0].body.items[0]
    assert.equal(item.content, 'USER: Where does Dana live?\n\nASSISTANT: Lyon.')
    assert.equal(item.document_id, 'chat-7')
    assert.equal(item.update_mode, 'append')
    assert.equal(item.context, 'conversation:openai:m1')
    assert.deepEqual(item.metadata, { source: 'openai-wrapper', model: 'm1' })
    assert.equal(llm.models.list(), 'passthrough')
  })

  it('fails open when recall fails and strips per-call overrides', async () => {
    route('POST', '/api/banks/support/memories/recall', { detail: 'boom' }, 500)
    const openai = fakeOpenAI()
    const errors = []
    const llm = withMemory(openai, bank, { onError: (e) => errors.push(e) })
    const out = await llm.chat.completions.create({ model: 'm', messages: [{ role: 'user', content: 'hi' }] })
    assert.equal(out.choices[0].message.content, 'Lyon.')
    assert.ok(errors[0] instanceof GrimoireError)
    calls.length = 0
    await llm.chat.completions.create({ model: 'm', messages: [{ role: 'user', content: 'hi' }], grimoire: { inject: false, store: false } })
    assert.equal(calls.length, 0)
    assert.deepEqual(Object.keys(openai.seen.at(-1)), ['model', 'messages'])
  })

  it('does not await a background retain and keeps its errors', async () => {
    route('POST', '/api/banks/support/memories/recall', { results: [] })
    route('POST', '/api/banks/support/memories', { detail: 'disk full' }, 500)
    const llm = withMemory(fakeOpenAI(), bank, { background: true })
    await llm.chat.completions.create({ model: 'm', messages: [{ role: 'user', content: 'hello' }] })
    await llm.flush()
    const errors = llm.pendingErrors()
    assert.equal(errors.length, 1)
    assert.match(String(errors[0]), /disk full/)
  })

  it('retains a stream once it is exhausted', async () => {
    route('POST', '/api/banks/support/memories/recall', { results: [] })
    const llm = withMemory(fakeOpenAI(), bank)
    const stream = await llm.chat.completions.create({ model: 'm', stream: true, messages: [{ role: 'user', content: 'where?' }] })
    assert.equal(find('POST', '/api/banks/support/memories').length, 0)
    for await (const _ of stream) { /* drain */ }
    assert.equal(find('POST', '/api/banks/support/memories')[0].body.items[0].content, 'USER: where?\n\nASSISTANT: Lyon')
  })

  it('formats human and disputed facts', () => {
    const text = formatMemories([{ text: 'a', authority: 'human' }, { text: 'b', type: 'experience', disputed_by: 'h' }, { text: ' ' }])
    assert.match(text, /1\. \[WORLD\] a \(written by a person\)/)
    assert.match(text, /2\. \[EXPERIENCE\] b \(disputed/)
    assert.equal(formatMemories([]), '')
  })
})
