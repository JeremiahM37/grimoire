/**
 * Memory banks for the Grimoire client, and a memory wrapper for any
 * OpenAI-compatible chat client.
 *
 *     import Grimoire from '@jeremiahm37/grimoire'
 *     import { Bank, Banks, withMemory } from '@jeremiahm37/grimoire/banks'
 *
 *     const g = new Grimoire('http://localhost:9111', { token })
 *     const bank = new Bank(g, 'support')
 *     await bank.retain('Dana moved the migration to May.', { documentId: 'chat-1' })
 *     const { results } = await bank.recall('when is the migration?')
 *
 *     const llm = withMemory(new OpenAI(), bank, { sessionId: 'chat-7' })
 *     await llm.chat.completions.create({ model, messages })
 *
 * A bank takes raw content and extracts the facts itself; facts a person
 * corrected outrank what a model extracted. See docs/MEMORY_BANKS.md.
 *
 * Same rules as index.js: no dependencies, the platform's fetch, and the
 * client's own `fetch` option is honoured.
 */

import { GrimoireError, NotFound, Unauthorized, VaultLocked } from './index.js'

/**
 * The server does not have this bank feature (yet). Raised by the newer
 * endpoints — reflect, observations, mental models, operations, webhooks,
 * templates — when the route itself is missing. A missing bank is still
 * NotFound.
 */
export class NotAvailable extends NotFound {
  constructor(...args) {
    super(...args)
    this.name = 'NotAvailable'
  }
}

const seg = (value) => encodeURIComponent(String(value))

function qs(params) {
  const q = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '' || value === false) continue
    q.set(key, String(value))
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

function iso(value) {
  return value instanceof Date ? value.toISOString() : value
}

function messageOf(text) {
  try {
    const parsed = JSON.parse(text)
    for (const key of ['error', 'message', 'detail']) {
      if (parsed?.[key]) return String(parsed[key])
    }
    return JSON.stringify(parsed)
  } catch {
    return text.trim() || 'request failed'
  }
}

function errorFor(status, message, url) {
  if (status === 404) return new NotFound(status, message, url)
  if (status === 401 || status === 403) return new Unauthorized(status, message, url)
  if (status === 423) return new VaultLocked(status, message, url)
  return new GrimoireError(status, message, url)
}

/** One HTTP call through a Grimoire client's url, token, agent and fetch. */
export async function bankRequest(client, method, path, body) {
  const url = client.url + path
  const headers = { Accept: 'application/json' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (client.token) headers.Authorization = `Bearer ${client.token}`
  if (client.agent) headers['X-Grimoire-Agent'] = client.agent
  let response
  try {
    response = await client._fetch(url, {
      method, headers, body: body === undefined ? undefined : JSON.stringify(body),
    })
  } catch (cause) {
    throw new GrimoireError(0, `cannot reach grimoire: ${cause.message}`, url)
  }
  const text = await response.text()
  if (!response.ok) throw errorFor(response.status, messageOf(text), url)
  if (!text) return null
  try {
    return JSON.parse(text)
  } catch {
    throw new GrimoireError(0, 'response was not json', url)
  }
}

/**
 * A call to a route the server may not have. An unknown route answers 404
 * with the router's plain "404 page not found" (or 405 when the path exists
 * for another method); a missing bank answers JSON detail and stays NotFound.
 */
export async function featureRequest(client, method, path, body) {
  try {
    return await bankRequest(client, method, path, body)
  } catch (error) {
    const missingRoute = error instanceof NotFound && /page not found/i.test(error.detail ?? '')
    if (missingRoute || error?.status === 405) {
      throw new NotAvailable(404, 'not available on this server', error.url)
    }
    throw error
  }
}

export class Bank {
  /** @param {import('./index.js').Grimoire} client @param {string} id */
  constructor(client, id) {
    this.client = client
    this.id = id
  }

  #req(method, path, body) {
    return bankRequest(this.client, method, `/api/banks/${seg(this.id)}${path}`, body)
  }

  #feature(method, path, body) {
    return featureRequest(this.client, method, `/api/banks/${seg(this.id)}${path}`, body)
  }

  // ---- profile ------------------------------------------------------

  profile() { return this.#req('GET', '') }
  /** Patch name, mission, retain_mission, disposition, directives, tags, config. */
  update(fields) { return this.#req('PATCH', '', fields) }
  delete() { return this.#req('DELETE', '') }

  // ---- retain / recall ------------------------------------------------

  /**
   * Hand the bank raw content (text, or an array of {speaker, text,
   * timestamp} turns). `async: true` asks for a queued retain; a server
   * without the operations queue refuses that, and the retain then runs
   * synchronously with `async_fallback: true` on the result.
   */
  async retain(content, options = {}) {
    let items = options.items
    if (!items) {
      if (content === undefined || content === null) throw new TypeError('retain needs content or items')
      const item = { content }
      const fields = [
        ['document_id', options.documentId], ['timestamp', iso(options.timestamp)],
        ['context', options.context], ['tags', options.tags], ['metadata', options.metadata],
        ['entities', options.entities], ['update_mode', options.updateMode],
      ]
      for (const [key, value] of fields) if (value !== undefined && value !== null) item[key] = value
      items = [item]
    }
    const body = { items }
    if (options.documentTags?.length) body.document_tags = options.documentTags
    if (options.mode) body.mode = options.mode
    if (!options.async) return this.#req('POST', '/memories', body)
    try {
      return await this.#req('POST', '/memories', { ...body, async: true })
    } catch (error) {
      if (error?.status !== 400 || !/async/i.test(error.detail ?? '')) throw error
    }
    const out = await this.#req('POST', '/memories', body)
    return out && typeof out === 'object' ? { ...out, async_fallback: true } : out
  }

  /** Recall facts relevant to `query`, packed into `maxTokens`. */
  recall(query, options = {}) {
    const body = { query }
    const fields = [
      ['budget', options.budget], ['max_tokens', options.maxTokens], ['types', options.types],
      ['tags', options.tags], ['tags_match', options.tagsMatch],
      ['query_timestamp', iso(options.queryTimestamp)],
    ]
    for (const [key, value] of fields) if (value !== undefined && value !== null) body[key] = value
    const include = {}
    if (options.includeEntities === false) include.entities = null
    if (options.includeChunks) {
      include.chunks = typeof options.includeChunks === 'number' ? { max_tokens: options.includeChunks } : {}
    }
    if (options.includeSourceFacts) include.source_facts = {}
    if (Object.keys(include).length) body.include = include
    if (options.trace) body.trace = true
    return this.#req('POST', '/memories/recall', body)
  }

  // ---- browse ----------------------------------------------------------

  listMemories(options = {}) {
    return this.#req('GET', '/memories' + qs({
      q: options.q, type: options.type, document_id: options.documentId,
      authority: options.authority, limit: options.limit, offset: options.offset,
    }))
  }
  memory(id) { return this.#req('GET', `/memories/${seg(id)}`) }
  /** A fact a person wrote needs `{ force: true }`. */
  deleteMemory(id, options = {}) { return this.#req('DELETE', `/memories/${seg(id)}${qs({ force: options.force })}`) }
  async entities(options = {}) {
    const out = await this.#req('GET', '/entities' + qs({ q: options.q, limit: options.limit }))
    return out?.items ?? []
  }
  entity(idOrName, options = {}) { return this.#req('GET', `/entities/${seg(idOrName)}${qs({ limit: options.limit })}`) }
  documents(options = {}) { return this.#req('GET', '/documents' + qs({ limit: options.limit, offset: options.offset })) }
  document(id) { return this.#req('GET', `/documents/${seg(id)}`) }
  deleteDocument(id, options = {}) { return this.#req('DELETE', `/documents/${seg(id)}${qs({ force: options.force })}`) }
  chunk(id) { return this.#req('GET', `/chunks/${seg(id)}`) }

  // ---- newer endpoints -------------------------------------------------
  // Routes a server may not have yet; each throws NotAvailable when missing.
  // Keep them together: this block is the one place to align later.

  reflect(query, options = {}) {
    const body = { query }
    const fields = [
      ['budget', options.budget], ['max_tokens', options.maxTokens], ['context', options.context],
      ['response_schema', options.responseSchema], ['types', options.types], ['tags', options.tags],
      ['tags_match', options.tagsMatch],
    ]
    for (const [key, value] of fields) if (value !== undefined && value !== null) body[key] = value
    const include = {}
    if (options.includeFacts !== false) include.facts = {}
    if (options.includeToolCalls) include.tool_calls = {}
    if (Object.keys(include).length) body.include = include
    return this.#feature('POST', '/reflect', body)
  }
  observations(options = {}) { return this.#feature('GET', '/observations' + qs(options)) }
  consolidate() { return this.#feature('POST', '/consolidate', {}) }
  mentalModels() { return this.#feature('GET', '/mental-models') }
  mentalModel(id) { return this.#feature('GET', `/mental-models/${seg(id)}`) }
  createMentalModel(name, sourceQuery, options = {}) {
    const body = { name, source_query: sourceQuery, tags: options.tags ?? [] }
    if (options.id) body.id = options.id
    if (options.maxTokens !== undefined) body.max_tokens = options.maxTokens
    if (options.refreshAfterConsolidation !== undefined) {
      body.trigger = { refresh_after_consolidation: options.refreshAfterConsolidation }
    }
    return this.#feature('POST', '/mental-models', body)
  }
  refreshMentalModel(id) { return this.#feature('POST', `/mental-models/${seg(id)}/refresh`, {}) }
  acceptProposal(id) { return this.#feature('POST', `/mental-models/${seg(id)}/proposal/accept`, {}) }
  rejectProposal(id) { return this.#feature('POST', `/mental-models/${seg(id)}/proposal/reject`, {}) }
  deleteMentalModel(id) { return this.#feature('DELETE', `/mental-models/${seg(id)}`) }
  operations(options = {}) { return this.#feature('GET', '/operations' + qs(options)) }
  operation(id) { return this.#feature('GET', `/operations/${seg(id)}`) }
  cancelOperation(id) { return this.#feature('DELETE', `/operations/${seg(id)}`) }
  webhooks() { return this.#feature('GET', '/webhooks') }
  createWebhook(url, options = {}) {
    const body = { url, enabled: options.enabled ?? true }
    if (options.secret) body.secret = options.secret
    if (options.eventTypes?.length) body.event_types = options.eventTypes
    return this.#feature('POST', '/webhooks', body)
  }
  updateWebhook(id, fields) { return this.#feature('PATCH', `/webhooks/${seg(id)}`, fields) }
  deleteWebhook(id) { return this.#feature('DELETE', `/webhooks/${seg(id)}`) }
}

/** The bank collection. */
export class Banks {
  constructor(client) { this.client = client }
  bank(id) { return new Bank(this.client, id) }
  async list() { return (await bankRequest(this.client, 'GET', '/api/banks'))?.banks ?? [] }
  create(id, fields = {}) { return bankRequest(this.client, 'POST', '/api/banks', { bank_id: id, ...fields }) }
  async templates() { return (await featureRequest(this.client, 'GET', '/api/bank-templates'))?.templates ?? [] }
}

// ---- memory wrapper ------------------------------------------------------

export const MEMORY_HEADING = '# Relevant memories'

function textOf(content) {
  if (typeof content === 'string') return content
  if (Array.isArray(content)) return content.filter((p) => p?.type === 'text' && p.text).map((p) => p.text).join(' ')
  return ''
}

export function lastUserText(messages = []) {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i]?.role === 'user') return textOf(messages[i].content).trim()
  }
  return ''
}

function when(fact) {
  const { occurred_start: s, occurred_end: e, mentioned_at: m } = fact
  if (s && e && s !== e) return `(occurred: ${s} to ${e}) `
  if (s) return `(occurred: ${s}) `
  if (m) return `(mentioned: ${m}) `
  return ''
}

/** The injected block, or '' when nothing was recalled. */
export function formatMemories(results = [], options = {}) {
  let rows = results.filter((r) => (r?.text ?? '').trim())
  if (options.maxMemories != null) rows = rows.slice(0, options.maxMemories)
  if (!rows.length) return ''
  const now = (options.now ?? new Date()).toISOString().replace('T', ' ').slice(0, 19)
  const lines = [MEMORY_HEADING, `Current date/time: ${now} UTC`,
    'The following information from memory may be relevant:', '']
  rows.forEach((fact, i) => {
    const note = fact.authority === 'human' ? ' (written by a person)' : ''
    const disputed = fact.disputed_by ? " (disputed by a person's correction)" : ''
    lines.push(`${i + 1}. [${(fact.type || 'world').toUpperCase()}] ${when(fact)}${fact.text.trim()}${note}${disputed}`)
  })
  return lines.join('\n')
}

export function injectMemories(messages, block) {
  const out = [...messages]
  const i = out.findIndex((m) => m?.role === 'system')
  if (i >= 0) {
    const existing = textOf(out[i].content)
    out[i] = { ...out[i], content: existing ? `${existing}\n\n${block}` : block }
    return out
  }
  return [{ role: 'system', content: block }, ...out]
}

const DEFAULTS = {
  inject: true, store: true, budget: 'mid', maxTokens: 4096, types: undefined,
  recallTags: undefined, recallTagsMatch: undefined, maxMemories: undefined, tags: undefined,
  sessionId: undefined, query: undefined, background: false, onError: undefined,
}

/**
 * Wrap an OpenAI-compatible client so `chat.completions.create` recalls from
 * `bank` before the call and retains the exchange after it. Per-call
 * overrides: a `grimoire` object in the request (`{ grimoire: { inject:
 * false } }`), removed before the request reaches the model. A failed recall
 * never breaks the call. With `background: true` the retain is not awaited;
 * its errors go to `onError` and `pendingErrors()`.
 */
export function withMemory(client, bank, options = {}) {
  const settings = { ...DEFAULTS, ...options }
  const errors = []
  const inflight = new Set()
  const report = (error) => {
    if (settings.onError) settings.onError(error)
    errors.push(error)
  }

  async function recallBlock(s, messages) {
    const query = s.query || lastUserText(messages)
    if (!s.inject || !query) return ''
    try {
      const out = await bank.recall(query.slice(0, 2000), {
        budget: s.budget, maxTokens: s.maxTokens, types: s.types, tags: s.recallTags,
        tagsMatch: s.recallTags ? s.recallTagsMatch : undefined,
      })
      return formatMemories(out?.results ?? [], { maxMemories: s.maxMemories })
    } catch (error) {
      if (s.onError) s.onError(error)
      return ''
    }
  }

  async function retainExchange(s, model, user, assistant) {
    if (!s.store || !user.trim() || !assistant.trim()) return
    const content = `USER: ${user}\n\nASSISTANT: ${assistant}`
    const base = {
      timestamp: new Date().toISOString(), context: `conversation:openai:${model || 'unknown'}`,
      metadata: { source: 'openai-wrapper', model: model || '' }, tags: s.tags,
    }
    if (s.sessionId) {
      try {
        await bank.retain(content, { ...base, documentId: s.sessionId, updateMode: 'append' })
        return
      } catch (error) {
        if (error?.status !== 400) throw error
      }
      await bank.retain(content, { ...base, documentId: s.sessionId })
      return
    }
    await bank.retain(content, base)
  }

  function store(s, model, user, assistant) {
    const job = retainExchange(s, model, user, assistant).catch(report)
    if (!s.background) return job
    inflight.add(job)
    job.finally(() => inflight.delete(job))
    return undefined
  }

  async function* streamed(stream, s, model, user) {
    let text = ''
    try {
      for await (const chunk of stream) {
        text += chunk?.choices?.[0]?.delta?.content ?? ''
        yield chunk
      }
    } finally {
      await store(s, model, user, text)
    }
  }

  async function create(request = {}, ...rest) {
    const { grimoire: overrides, ...params } = request
    const s = { ...settings, ...(overrides ?? {}) }
    const messages = params.messages ?? []
    const user = lastUserText(messages)
    const block = await recallBlock(s, messages)
    if (block) params.messages = injectMemories(messages, block)
    const response = await client.chat.completions.create(params, ...rest)
    if (params.stream) return streamed(response, s, params.model, user)
    const reply = textOf(response?.choices?.[0]?.message?.content)
    await store(s, params.model, user, reply)
    return response
  }

  // Methods are bound to the real object: a client that keeps #private
  // state would throw if called with the proxy as `this`.
  const pass = (target, prop) => {
    const value = Reflect.get(target, prop)
    return typeof value === 'function' ? value.bind(target) : value
  }
  const completions = new Proxy(client.chat.completions, {
    get: (target, prop) => (prop === 'create' ? create : pass(target, prop)),
  })
  const chat = new Proxy(client.chat, {
    get: (target, prop) => (prop === 'completions' ? completions : pass(target, prop)),
  })
  return new Proxy(client, {
    get(target, prop) {
      if (prop === 'chat') return chat
      if (prop === 'pendingErrors') return () => errors.splice(0)
      if (prop === 'flush') return () => Promise.allSettled([...inflight])
      if (prop === 'memorySettings') return settings
      return pass(target, prop)
    },
  })
}
