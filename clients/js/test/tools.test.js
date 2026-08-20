// The AI SDK tool adapter.
//
// The shape tests run everywhere. The integration test only runs where the
// `ai` package is installed, and it is the one that matters: it hands the
// tools to a real generateText call driven by a mock model, so an argument
// name or a schema the SDK rejects fails here rather than in someone's app.

import assert from 'node:assert/strict'
import { after, beforeEach, describe, it } from 'node:test'
import { createServer } from 'node:http'

import Grimoire from '../index.js'
import { grimoireTools } from '../tools.js'

const calls = []
let reply = {}

const server = createServer((req, res) => {
  const chunks = []
  req.on('data', (c) => chunks.push(c))
  req.on('end', () => {
    const raw = Buffer.concat(chunks).toString()
    calls.push({ method: req.method, path: req.url, body: raw ? JSON.parse(raw) : undefined })
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify(reply))
  })
})
await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
after(() => server.close())

const last = () => calls[calls.length - 1]

let client
let tools
beforeEach(() => {
  calls.length = 0
  reply = {}
  client = new Grimoire(base, { agent: 'tools-test' })
  tools = grimoireTools(client)
})

describe('tool definitions', () => {
  it('gives every tool a description, a schema and an executor', () => {
    for (const [name, tool] of Object.entries(tools)) {
      assert.ok(tool.description.length > 60, `${name}: description too thin to steer a model`)
      assert.equal(tool.inputSchema.type, 'object', `${name}: schema is not an object`)
      assert.equal(typeof tool.execute, 'function', `${name}: not executable`)
    }
  })

  it('says what changes behaviour, not just what is callable', () => {
    // A model told only that it can write memory will hedge a correction into
    // a second fact competing with the first.
    assert.match(tools.rememberFact.description, /RECONCILE/)
    assert.match(tools.recallMemory.description, /currently-believed/)
    assert.match(tools.forgetFact.description, /WRONG/)
  })

  it('marks the arguments a tool cannot work without', () => {
    assert.deepEqual(tools.rememberFact.inputSchema.required, ['text'])
    assert.deepEqual(tools.forgetFact.inputSchema.required, ['id', 'path'])
    assert.deepEqual(tools.rateMemory.inputSchema.required, ['id', 'path', 'helpful'])
    assert.deepEqual(tools.recallMemory.inputSchema.required, [])
  })

  it('wraps schemas with a supplied helper', () => {
    const wrapped = grimoireTools(client, { jsonSchema: (s) => ({ wrapped: s }) })
    assert.ok(wrapped.recallMemory.inputSchema.wrapped, 'the helper was not applied')
  })

  it('can build a subset, and rejects a name it does not have', () => {
    const subset = grimoireTools(client, { include: ['recallMemory'] })
    assert.deepEqual(Object.keys(subset), ['recallMemory'])
    assert.throws(() => grimoireTools(client, { include: ['nope'] }), /unknown grimoire tool/)
  })
})

describe('tool execution', () => {
  it('recalls', async () => {
    reply = [{ id: 'a1', text: 'the user prefers tabs' }]
    const out = await tools.recallMemory.execute({ query: 'indentation', limit: 3 })
    assert.equal(out[0].text, 'the user prefers tabs')
    assert.ok(last().path.includes('q=indentation'))
    assert.ok(last().path.includes('limit=3'))
  })

  it('remembers, and passes the scope through', async () => {
    reply = { op: 'ADD', results: [{ op: 'ADD', id: 'a1' }] }
    const out = await tools.rememberFact.execute({
      text: 'the user prefers tabs', topic: 'prefs', session: 'run-1',
    })
    assert.equal(out.op, 'ADD')
    assert.equal(last().body.topic, 'prefs')
    assert.equal(last().body.session, 'run-1')
  })

  it('forgets, graphs and rates', async () => {
    reply = {}
    await tools.forgetFact.execute({ id: 'a1', path: 'memory/x.md' })
    assert.equal(last().method, 'DELETE')

    reply = { seed: 'priya', nodes: [], edges: [], entries: [] }
    await tools.memoryGraph.execute({ entity: 'priya', depth: 2 })
    assert.ok(last().path.includes('/api/memory/graph'))
    assert.ok(last().path.includes('depth=2'))

    reply = { helpful: 1 }
    await tools.rateMemory.execute({ id: 'a1', path: 'memory/x.md', helpful: true })
    assert.equal(last().path, '/api/memory/feedback')
    assert.equal(last().body.helpful, true)
  })

  it('asks the knowledge base', async () => {
    reply = { answer: '8443' }
    await tools.askNotes.execute({ question: 'what port', limit: 3 })
    assert.deepEqual(last().body, { question: 'what port', k: 3 })
  })

  it('defaults the arguments a model left out', async () => {
    reply = []
    await tools.recallMemory.execute({})
    assert.ok(last().path.includes('limit=10'))

    reply = { seed: '', nodes: [], edges: [], entries: [] }
    await tools.memoryGraph.execute({})
    assert.ok(last().path.includes('depth=1'))
  })
})

// The integration test. Skipped where `ai` is not installed; it is what proves
// the schemas and argument names survive the SDK rather than only my reading
// of its docs.
let ai
try {
  ai = await import('ai')
} catch {
  ai = null
}

describe('against the real AI SDK', { skip: ai ? false : 'the ai package is not installed' }, () => {
  it('is accepted by generateText and executed with the model\'s arguments', async () => {
    const { generateText, jsonSchema, stepCountIs } = ai
    const { MockLanguageModelV3 } = await import('ai/test')

    // Its own server and its own request log. The suites in this file run
    // concurrently, and the shared stub's `reply` is reset by every other
    // test's beforeEach — which reaches this one mid-flight and turns its tool
    // call into a tool ERROR, so `toolResults` comes back empty and the
    // failure looks like the adapter's fault.
    const seen = []
    const own = createServer((req, res) => {
      seen.push(req.url)
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify([
        { id: 'a1', text: 'the user prefers tabs', path: 'memory/prefs.md' },
      ]))
    })
    await new Promise((resolve) => own.listen(0, '127.0.0.1', resolve))
    const ownClient = new Grimoire(`http://127.0.0.1:${own.address().port}`)
    const sdkTools = grimoireTools(ownClient, { jsonSchema, include: ['recallMemory'] })

    let step = 0
    const model = new MockLanguageModelV3({
      doGenerate: async () => (step++ === 0
        ? {
            // The provider spec takes a unified reason plus the provider's
            // raw one; a bare string leaves the loop unable to read it, which
            // silently produces a run where the tool is never executed.
            finishReason: { unified: 'tool-calls', raw: 'tool_calls' },
            usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 },
            content: [{
              type: 'tool-call',
              toolCallId: 'call-1',
              toolName: 'recallMemory',
              input: JSON.stringify({ query: 'indentation', limit: 3 }),
            }],
          }
        : {
            finishReason: { unified: 'stop', raw: 'stop' },
            usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 },
            content: [{ type: 'text', text: 'They prefer tabs.' }],
          }),
    })

    let result
    try {
      result = await generateText({
        model,
        tools: sdkTools,
        stopWhen: stepCountIs(3),
        prompt: 'what indentation does the user prefer?',
      })
    } finally {
      own.close()
    }

    assert.equal(result.text, 'They prefer tabs.')
    // The tool actually ran, against the server, with the model's arguments.
    assert.equal(seen.length, 1, `requests: ${seen}`)
    assert.ok(seen[0].includes('q=indentation'), seen[0])
    assert.ok(seen[0].includes('limit=3'), seen[0])
    assert.equal(result.toolResults[0].toolName, 'recallMemory')
    assert.equal(result.toolResults[0].output[0].text, 'the user prefers tabs')
  })
})
