// Tests for contextFor / ContextSession.
//
// The fetch is injected, so what is asserted is the request the client builds
// and which replies it accepts: the same rules as clients/hooks/grimoire_context.py.

import assert from 'node:assert/strict'
import { beforeEach, describe, it } from 'node:test'

import Grimoire, { ContextSession, contextFor } from '../index.js'

const KEY = 'a'.repeat(32)
const calls = []
let reply
let failure

const fakeFetch = async (url, init) => {
  calls.push({ url: new URL(url), init })
  if (failure) throw failure
  const body = typeof reply === 'string' ? reply : JSON.stringify(reply)
  return new Response(body, { status: 200 })
}
const client = new Grimoire('http://127.0.0.1:9111', { token: 'tok', fetch: fakeFetch })

beforeEach(() => {
  calls.length = 0
  reply = { context: 'reference', keys: [KEY] }
  failure = null
})

describe('contextFor', () => {
  it('calls the hook endpoint with its parameters', async () => {
    const got = await contextFor('How does kestrel deploy?', {
      client, paths: ['memory/kestrel.md'], exclude: [KEY], budget: 1000,
    })
    assert.equal(got, 'reference')
    const { url, init } = calls[0]
    assert.equal(url.pathname, '/api/memory/context')
    assert.equal(init.headers.Authorization, 'Bearer tok')
    assert.equal(url.searchParams.get('scope'), 'scoped')
    assert.deepEqual(url.searchParams.getAll('path'), ['memory/kestrel.md'])
    assert.equal(url.searchParams.get('max_bytes'), '1000')
    assert.equal(url.searchParams.get('exclude'), KEY)
    assert.equal(url.searchParams.get('limit'), '5')
  })

  it('makes no request for trivial prompts', async () => {
    for (const prompt of ['', '  ', 'thanks', 'Thanks!', 'ok', 'continue', 'x'.repeat(8001)]) {
      assert.equal(await contextFor(prompt, { client }), '')
    }
    assert.equal(calls.length, 0)
  })

  it('never widens an empty scope', async () => {
    assert.equal(await contextFor('kestrel?', { client, mode: 'scoped' }), '')
    assert.equal(await contextFor('kestrel?', { client, mode: 'all', paths: ['a.md'] }), '')
    assert.equal(calls.length, 0)
  })

  it('defaults to all without paths', async () => {
    await contextFor('kestrel?', { client })
    assert.equal(calls[0].url.searchParams.get('scope'), 'all')
  })

  it('drops invalid replies', async () => {
    for (const bad of [
      { context: 'x'.repeat(129), keys: [] },
      { context: 'ok', keys: Array(11).fill(KEY) },
      { context: 'ok', keys: ['not-hex'] },
      { context: 5, keys: [] },
      'not json',
    ]) {
      reply = bad
      assert.equal(await contextFor('kestrel?', { client, budget: 128 }), '')
    }
  })

  it('never throws when the server is unreachable', async () => {
    failure = new Error('connection refused')
    assert.equal(await contextFor('kestrel?', { client }), '')
    assert.equal(await new ContextSession({ client }).contextFor('kestrel?'), '')
  })
})

describe('ContextSession', () => {
  it('deduplicates like the hook', async () => {
    let now = 100_000
    const session = new ContextSession({ client, now: () => now })
    assert.equal(await session.contextFor('How does kestrel deploy?'), 'reference')
    now += 1000
    assert.equal(await session.contextFor('How does kestrel deploy?'), '')   // repeat < 30s
    assert.equal(calls.length, 1)
    now += 1000
    reply = { context: '', keys: [] }
    await session.contextFor('Kestrel certificates?')
    assert.equal(calls.at(-1).url.searchParams.get('exclude'), KEY)          // fact dedup
    now += 1_801_000
    await session.contextFor('Kestrel ports?')
    assert.equal(calls.at(-1).url.searchParams.get('exclude'), '')           // 30-min expiry
  })

  it('retries after a failed lookup', async () => {
    const session = new ContextSession({ client, now: () => 1 })
    reply = 'garbage'
    assert.equal(await session.contextFor('kestrel deployment?'), '')
    reply = { context: 'reference', keys: [] }
    assert.equal(await session.contextFor('kestrel deployment?'), 'reference')
  })
})
