/**
 * Grimoire client — talk to a Grimoire server from Node or the browser.
 *
 * No dependencies and no build step: the source is the published artifact,
 * standard ESM against the platform's `fetch`. The server is one static binary
 * with no runtime, and a client that needs a toolchain to reach it undoes the
 * property people install it for. TypeScript users get full types from the
 * hand-written `index.d.ts` beside this file.
 *
 * The memory surface comes first, with mem0-compatible names
 * (`add`/`search`/`getAll`/`delete`) beside the native ones, so switching a
 * script over is an import change rather than a rewrite — with one behavioural
 * difference worth knowing: `add` reconciles. A fact contradicting one already
 * on file supersedes it, and a fact already recorded writes nothing. The
 * result says which happened.
 */

export class GrimoireError extends Error {
  constructor(status, message, url = '') {
    super(`${status} ${message}${url ? ` (${url})` : ''}`)
    this.name = 'GrimoireError'
    this.status = status
    this.detail = message
    this.url = url
  }
}

/**
 * The note, fact or entry does not exist — or is not readable by you. The
 * server does not distinguish those: telling an unauthorized caller that
 * something exists is itself a disclosure.
 */
export class NotFound extends GrimoireError {
  constructor(...args) {
    super(...args)
    this.name = 'NotFound'
  }
}

export class Unauthorized extends GrimoireError {
  constructor(...args) {
    super(...args)
    this.name = 'Unauthorized'
  }
}

/** The credential vault is locked; a human has to unlock it. */
export class VaultLocked extends GrimoireError {
  constructor(...args) {
    super(...args)
    this.name = 'VaultLocked'
  }
}

function errorFor(status, message, url) {
  if (status === 404) return new NotFound(status, message, url)
  if (status === 401 || status === 403) return new Unauthorized(status, message, url)
  if (status === 423) return new VaultLocked(status, message, url)
  return new GrimoireError(status, message, url)
}

/** Whether a write stored a new fact, as opposed to NOOP or DELETE. */
export function stored(result) {
  return result?.op === 'ADD' || result?.op === 'UPDATE'
}

/** Whether a recalled fact was later replaced. */
export function superseded(fact) {
  return Boolean(fact?.superseded_by)
}

export class Grimoire {
  /**
   * @param {string} url  base url of the server
   * @param {{token?: string, agent?: string, fetch?: typeof fetch}} [options]
   */
  constructor(url = 'http://localhost:9111', options = {}) {
    this.url = url.replace(/\/+$/, '')
    this.token = options.token ?? null
    this.agent = options.agent ?? 'js-client'
    // Injectable so tests — and a caller behind a proxy or with a retry
    // wrapper — do not have to monkey-patch the global.
    this._fetch = options.fetch ?? globalThis.fetch?.bind(globalThis)
    if (!this._fetch) {
      throw new Error('no fetch available; pass one via options.fetch')
    }
  }

  // ---- memory ---------------------------------------------------------

  /**
   * Record a fact, reconciled against what is already known.
   * Returns the first per-fact result; use `remember` for all of them.
   */
  async add(text, options = {}) {
    const response = await this.remember(text, options)
    const results = response?.results ?? []
    // `results` is authoritative — the top level repeats only enough of the
    // first entry for a caller that wants one line back, so reading the list
    // is what keeps `target` and `why` from silently going missing.
    return results.length ? results[0] : response
  }

  /** `add`, returning the raw response including every result. */
  async remember(text, options = {}) {
    const body = { text, agent: options.agent ?? this.agent }
    for (const key of ['topic', 'task', 'session', 'category', 'expires_in',
      'expires', 'scope', 'fresh', 'check']) {
      if (options[key]) body[key] = options[key]
    }
    if (options.immutable) body.immutable = true
    if (options.infer === false) body.infer = false
    return this.#request('POST', '/api/memory', body)
  }

  /**
   * Record several facts in one call. Each is reconciled against everything
   * written before it, so a batch that contradicts itself ends with the last
   * fact standing.
   */
  async addMany(items) {
    const payload = items.map((item) => ({ agent: this.agent, ...item }))
    const out = await this.#request('POST', '/api/memory/batch', { items: payload })
    return out?.results ?? []
  }

  /**
   * Recall facts, most relevant first. Only what is currently believed unless
   * you ask otherwise: `includeSuperseded` also returns replaced beliefs, and
   * `asOf` (an RFC3339 instant) reconstructs what was believed then.
   */
  async search(query = '', options = {}) {
    const params = new URLSearchParams({ limit: String(options.limit ?? 10) })
    if (query) params.set('q', query)
    for (const [key, value] of [
      ['agent', options.agent], ['session', options.session],
      ['category', options.category], ['path', options.path],
      ['as_of', options.asOf],
    ]) {
      if (value) params.set(key, value)
    }
    for (const [key, flag] of [
      ['include_superseded', options.includeSuperseded],
      ['include_expired', options.includeExpired],
      ['explain', options.explain],
    ]) {
      if (flag) params.set(key, '1')
    }
    return (await this.#request('GET', `/api/memory?${params}`)) ?? []
  }

  /** Every fact in scope, newest first. mem0-compatible alias. */
  async getAll(options = {}) {
    return this.search('', { limit: 200, ...options })
  }

  /**
   * What was believed at an instant. Answerable because a replaced fact is
   * struck through rather than deleted.
   */
  async history(asOf, options = {}) {
    return this.search(options.query ?? '', { ...options, asOf })
  }

  /** Edit one fact in place, leaving every other line of the note alone. */
  async update(path, id, changes = {}) {
    const body = { path, id }
    for (const key of ['text', 'category', 'expires', 'expires_in', 'immutable']) {
      // `immutable: false` is a value, not an absence — a truthiness test
      // here would silently refuse to unpin a fact.
      if (changes[key] !== undefined) body[key] = changes[key]
    }
    return this.#request('PATCH', '/api/memory/entry', body)
  }

  /**
   * Retract one fact. By default it stops being recalled but stays in the
   * note, struck through; `hard` removes the line.
   */
  async delete(path, id, options = {}) {
    const params = new URLSearchParams({ path, id, agent: this.agent })
    if (options.hard) params.set('hard', '1')
    return this.#request('DELETE', `/api/memory/entry?${params}`)
  }

  /**
   * Report whether a recalled fact earned its place. A nudge in ranking, not a
   * verdict — for a fact that is *wrong*, use `delete`.
   */
  async feedback(path, id, helpful) {
    return this.#request('POST', '/api/memory/feedback', { path, id, helpful })
  }

  /**
   * What memory knows about a thing, and what it is connected to. Returns
   * nodes, edges naming the facts that connect them, and those facts — so an
   * edge can be read rather than trusted. With no entity, the busiest ones.
   */
  async graph(entity = '', options = {}) {
    const params = new URLSearchParams({
      depth: String(options.depth ?? 1),
      limit: String(options.limit ?? 50),
    })
    for (const [key, value] of [
      ['entity', entity], ['agent', options.agent],
      ['session', options.session], ['category', options.category],
    ]) {
      if (value) params.set(key, value)
    }
    return this.#request('GET', `/api/memory/graph?${params}`)
  }

  /** The agents, sessions and categories memory has been recorded under. */
  async scopes() {
    return this.#request('GET', '/api/memory/facets')
  }

  /** Every fact you may read, superseded and expired ones included. */
  async export(filters = {}) {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(filters)) {
      if (value) params.set(key, value)
    }
    const query = params.toString()
    return this.#request('GET', `/api/memory/export${query ? `?${query}` : ''}`)
  }

  /** Compact the memory namespace. Snapshotted first, so it is reviewable. */
  async consolidate(options = {}) {
    const body = {}
    if (options.topic) body.topic = options.topic
    if (options.path) body.path = options.path
    return this.#request('POST', '/api/memory/consolidate', body)
  }

  // ---- knowledge ------------------------------------------------------

  /** Ask the knowledge base, with citations. */
  async ask(question, options = {}) {
    return this.#request('POST', '/api/ask', { question, k: options.limit ?? 8 })
  }

  /** Full-text search over notes. */
  async searchNotes(query, options = {}) {
    const params = new URLSearchParams({ q: query, limit: String(options.limit ?? 20) })
    return (await this.#request('GET', `/api/search?${params}`)) ?? []
  }

  /** One note, with its frontmatter. */
  async readNote(path) {
    return this.#request('GET', `/api/notes/${encodePath(path)}`)
  }

  /** Create or replace a note. */
  async writeNote(path, body, frontmatter) {
    const payload = { body }
    if (frontmatter) payload.frontmatter = frontmatter
    return this.#request('PUT', `/api/notes/${encodePath(path)}`, payload)
  }

  /** Standing context for an agent joining a session. */
  async briefing(options = {}) {
    return this.#request('GET', `/api/briefing?memories=${options.memories ?? 5}`)
  }

  /** Look up an exact recorded value rather than paraphrasing prose. */
  async getFact(key, options = {}) {
    const params = new URLSearchParams({ key })
    if (options.note) params.set('note', options.note)
    return (await this.#request('GET', `/api/facts?${params}`)) ?? []
  }

  /** Server liveness and corpus counts. */
  async health() {
    return this.#request('GET', '/api/health')
  }

  // ---- transport ------------------------------------------------------

  async #request(method, path, body) {
    const url = this.url + path
    const headers = { Accept: 'application/json' }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (this.token) headers.Authorization = `Bearer ${this.token}`

    let response
    try {
      response = await this._fetch(url, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      })
    } catch (cause) {
      throw new GrimoireError(0, `cannot reach grimoire: ${cause.message}`, url)
    }
    const text = await response.text()
    if (!response.ok) {
      throw errorFor(response.status, messageOf(text), url)
    }
    if (!text) return null
    try {
      return JSON.parse(text)
    } catch {
      // A body that is not JSON is a proxy or a captive portal answering
      // instead of the server; say so rather than throwing a parse error from
      // somewhere deep in the caller's code.
      throw new GrimoireError(0, 'response was not json', url)
    }
  }
}

// ---- pre-request context ------------------------------------------------
//
// The same read path and rules as the Claude Code / Codex hook
// (clients/hooks/grimoire_context.py) and grimoire_client.context in Python:
// GET /api/memory/context, lexical, no model calls. The hook is a standalone
// script, so its rules are mirrored here rather than imported.

const ACKNOWLEDGEMENT = /^(?:thanks?|thank you|ok(?:ay)?|yes|no|continue|proceed|hi|hello)[.!\s]*$/i
const FACT_KEY = /^[a-f0-9]{32}$/
const encoder = new TextEncoder()

function cleanPrompt(prompt) {
  if (typeof prompt !== 'string' || encoder.encode(prompt).length > 8000) return ''
  const trimmed = prompt.trim()
  return !trimmed || ACKNOWLEDGEMENT.test(trimmed) ? '' : trimmed
}

/** `{context, keys}`; `context` is null when no lookup succeeded. */
async function lookup(prompt, options = {}) {
  const query = cleanPrompt(prompt)
  if (!query) return { context: null, keys: [] }
  const client = options.client ?? new Grimoire(
    globalThis.process?.env?.GRIMOIRE_URL ?? 'http://localhost:9111',
    { token: globalThis.process?.env?.GRIMOIRE_AUTH_TOKEN, agent: 'context' })
  const paths = [...(options.paths ?? [])]
  const mode = options.mode ?? (paths.length ? 'scoped' : 'all')
  // Scoped needs paths, all takes none; an empty scope never widens.
  if (!['all', 'scoped'].includes(mode) || paths.length > 32
    || paths.some((p) => typeof p !== 'string' || p.length > 512)
    || (mode === 'scoped') !== (paths.length > 0)) {
    return { context: null, keys: [] }
  }
  const budget = Math.max(128, Math.min(8000, Math.trunc(options.budget ?? 2400)))
  const params = new URLSearchParams({
    q: query,
    exclude: [...(options.exclude ?? [])].slice(-256).join(','),
    max_bytes: String(budget),
    limit: '5',
    scope: mode,
  })
  for (const path of paths) params.append('path', path)
  const headers = { Accept: 'application/json' }
  if (client.token) headers.Authorization = `Bearer ${client.token}`
  try {
    const response = await client._fetch(`${client.url}/api/memory/context?${params}`, {
      method: 'GET', headers, redirect: 'error', signal: AbortSignal.timeout(1500),
    })
    if (!response.ok) return { context: null, keys: [] }
    const text = await response.text()
    if (text.length > 64000) return { context: null, keys: [] }
    const result = JSON.parse(text)
    const { context, keys } = result ?? {}
    if (typeof context !== 'string' || encoder.encode(context).length > budget
      || !Array.isArray(keys) || keys.length > 10
      || keys.some((k) => typeof k !== 'string' || !FACT_KEY.test(k))) {
      return { context: null, keys: [] }
    }
    return { context, keys }
  } catch {
    return { context: null, keys: [] }
  }
}

/**
 * Bounded reference context for a prompt, or `''` when nothing is relevant,
 * the prompt is trivial ("thanks"), or the server is unavailable. Never throws.
 * Stateless; use `ContextSession` to deduplicate across a conversation.
 *
 * @param {string} prompt
 * @param {{client?: Grimoire, budget?: number, mode?: 'all'|'scoped',
 *   paths?: string[], exclude?: string[]}} [options]
 */
export async function contextFor(prompt, options = {}) {
  return (await lookup(prompt, options)).context ?? ''
}

/**
 * Per-conversation deduplication with the hook's rules: no request for
 * acknowledgements, an identical query is skipped for 30 seconds, and a fact
 * is not repeated for 30 minutes. In memory only; `reset()` after a compaction.
 */
export class ContextSession {
  constructor(options = {}) {
    this.options = options
    this.now = options.now ?? Date.now
    this.reset()
  }

  reset() {
    this.seen = new Map()
    this.query = ''
    this.checked = 0
  }

  async contextFor(prompt) {
    const query = cleanPrompt(prompt)
    if (!query) return ''
    const now = this.now()
    if (this.query === query && now - this.checked < 30_000) return ''
    for (const [key, stamp] of this.seen) {
      if (now - stamp >= 1_800_000) this.seen.delete(key)
    }
    const { context, keys } = await lookup(query, { ...this.options, exclude: [...this.seen.keys()] })
    // A failed lookup is not remembered, so the next turn tries again.
    if (context === null) return ''
    this.query = query
    this.checked = now
    for (const key of keys) this.seen.set(key, now)
    while (this.seen.size > 256) this.seen.delete(this.seen.keys().next().value)
    return context
  }
}

/** Percent-encode a note path without destroying its separators. */
function encodePath(path) {
  return path.split('/').map(encodeURIComponent).join('/')
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

export default Grimoire
