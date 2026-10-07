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
 * The server does not have this bank feature. Raised by the reasoning
 * endpoints — reflect, observations, mental models, directives, operations,
 * webhooks, templates — on a server from before them, which answers 404 for
 * the route itself. A missing bank is still NotFound.
 */
export class NotAvailable extends NotFound {
  constructor(...args) {
    super(...args)
    this.name = 'NotAvailable'
  }
}

/**
 * The call needs a language model and the server has none configured
 * (409 `{code: 'model_required'}`): consolidation, a mental-model refresh.
 * Reflect answers extractively instead of throwing it.
 */
export class ModelRequired extends GrimoireError {
  constructor(...args) {
    super(...args)
    this.name = 'ModelRequired'
    this.code = 'model_required'
  }
}

/** Operation statuses after which nothing more happens. */
export const TERMINAL_STATUSES = ['completed', 'failed', 'cancelled']

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

function codeOf(text) {
  try {
    const code = JSON.parse(text)?.code
    return typeof code === 'string' ? code : ''
  } catch {
    return ''
  }
}

function errorFor(status, text, url) {
  const message = messageOf(text)
  if (status === 409 && codeOf(text) === 'model_required') return new ModelRequired(status, message, url)
  if (status === 404) return new NotFound(status, message, url)
  if (status === 401 || status === 403) return new Unauthorized(status, message, url)
  if (status === 423) return new VaultLocked(status, message, url)
  return new GrimoireError(status, message, url)
}

/** One HTTP call through a Grimoire client's url, token, agent and fetch. */
export async function bankRequest(client, method, path, body, options = {}) {
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
  if (!response.ok) throw errorFor(response.status, text, url)
  if (options.raw) return text
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
export async function featureRequest(client, method, path, body, options) {
  try {
    return await bankRequest(client, method, path, body, options)
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

  #feature(method, path, body, options) {
    return featureRequest(this.client, method, `/api/banks/${seg(this.id)}${path}`, body, options)
  }

  // ---- profile ------------------------------------------------------

  profile() { return this.#req('GET', '') }
  /** Patch name, mission, retain_mission, disposition, directives, tags, config. */
  update(fields) { return this.#req('PATCH', '', fields) }
  delete() { return this.#req('DELETE', '') }

  // ---- retain / recall ------------------------------------------------

  /**
   * Hand the bank raw content (text, or an array of {speaker, text,
   * timestamp} turns). `async: true` queues it: the server answers at once
   * with `{async: true, operation_id, operation_ids, items_count}`, and
   * `waitOperation` follows it. A server without the operations queue
   * refuses that, and the retain then runs synchronously with
   * `async_fallback: true` on the result.
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

  // ---- coding-agent surfaces -----------------------------------------------

  /** What a coding agent is shown at session start, rendered under `maxChars` (default 9000). */
  context(options = {}) {
    return this.#req('GET', '/context' + qs({ max_chars: options.maxChars, source: options.source }))
  }
  /** A compact, citable index, newest first or ranked by `query`. */
  index(query, options = {}) {
    return this.#req('GET', '/index' + qs({
      q: query, types: options.types?.join(','), since: options.since, limit: options.limit, offset: options.offset,
    }))
  }
  /** Entries dated around `anchor` (a `#ref` or `YYYY-MM-DD`), oldest first. */
  timeline(anchor, options = {}) {
    return this.#req('GET', '/timeline' + qs({ anchor, before: options.before, after: options.after }))
  }
  /** What the bank remembers about one file. */
  fileMemory(path, options = {}) { return this.#req('GET', '/file-memory' + qs({ path, limit: options.limit })) }
  /** Near-duplicate candidates for review. */
  duplicates(options = {}) {
    return this.#req('GET', '/duplicates' + qs({ min_score: options.minScore, type: options.type, limit: options.limit }))
  }
  /** Strike `merge` through into `keep`; its text is kept, never deleted. */
  mergeDuplicates(keep, merge) { return this.#req('POST', '/duplicates/merge', { keep, merge }) }
  /** Entries in full by `#ref` or id. */
  getEntries(ids) { return this.#req('GET', '/lookup' + qs({ ids: ids.join(',') })) }
  /** Write a session's "where we left off" note. */
  writeDigest(sessionId, turns, options = {}) {
    return this.#req('POST', `/sessions/${seg(sessionId)}/digest`, {
      turns, activity: options.activity ?? {}, use_model: !!options.useModel,
    })
  }
  sessions(options = {}) { return this.#req('GET', '/sessions' + qs({ limit: options.limit })) }

  // ---- reasoning endpoints ----------------------------------------------
  // Reflect, observations, mental models, directives, operations, webhooks
  // and templates. A server from before these routes throws NotAvailable;
  // a call that needs a language model the server lacks throws
  // ModelRequired (error.code === 'model_required').

  /** Counts, including `model_available`. */
  stats() { return this.#feature('GET', '/stats') }

  /**
   * Answer `query` by reasoning over the bank. The result always has `text`,
   * `mode` ('llm' or 'extractive') and `based_on` {memories, observations,
   * mental_models, directives}; `trace` only with `includeToolCalls` or
   * `trace: true`. `types` is accepted as an alias of `factTypes`.
   */
  reflect(query, options = {}) {
    const body = { query }
    const fields = [
      ['budget', options.budget], ['max_tokens', options.maxTokens], ['context', options.context],
      ['response_schema', options.responseSchema], ['fact_types', options.factTypes ?? options.types],
      ['tags', options.tags], ['tags_match', options.tagsMatch], ['tag_groups', options.tagGroups],
      ['apply_all_directives', options.applyAllDirectives],
      ['exclude_mental_models', options.excludeMentalModels],
      ['exclude_mental_model_ids', options.excludeMentalModelIds],
      ['query_timestamp', iso(options.queryTimestamp)],
    ]
    for (const [key, value] of fields) if (value !== undefined && value !== null) body[key] = value
    const include = {}
    if (options.includeFacts !== false) include.facts = {}
    if (options.includeToolCalls) include.tool_calls = {}
    if (Object.keys(include).length) body.include = include
    if (options.trace) body.trace = true
    return this.#feature('POST', '/reflect', body)
  }

  // observations and consolidation

  /** `{items, total}`, plus `history` with `includeHistory`. */
  observations(options = {}) {
    return this.#feature('GET', '/observations' + qs({
      q: options.q, authority: options.authority, tags: options.tags?.join(','),
      tags_match: options.tagsMatch, include_history: options.includeHistory ? 'true' : undefined,
      limit: options.limit, offset: options.offset,
    }))
  }
  /** `{observation, history}`. */
  observation(id) { return this.#feature('GET', `/observations/${seg(id)}`) }
  /** Retire one into history; a person's needs `{ force: true }`. */
  deleteObservation(id, options = {}) { return this.#feature('DELETE', `/observations/${seg(id)}${qs({ force: options.force })}`) }
  /** Retire every model observation (a person's stay). */
  clearObservations() { return this.#feature('DELETE', '/observations') }
  /** Queue a consolidation: `{operation_id, deduplicated}`. */
  consolidate() { return this.#feature('POST', '/consolidate', {}) }

  // mental models — ids are paths ('people/dana'), one encoded segment

  /** `{items, total}`; bodies with `detail: true`. */
  mentalModels(options = {}) {
    return this.#feature('GET', '/mental-models' + qs({
      tags: options.tags?.join(','), tags_match: options.tagsMatch, folder: options.folder,
      detail: options.detail ? 'full' : undefined,
    }))
  }
  mentalModel(id) { return this.#feature('GET', `/mental-models/${seg(id)}`) }
  /**
   * Create a standing question. Returns `{mental_model, mental_model_id,
   * operation_id}`; `operation_id` is null when `body` was given or the
   * server has no model. `id` is the whole path; without one the id is
   * `folder` plus a slug of `name`. `options.sourceQuery` and
   * `refreshAfterConsolidation` are accepted from older callers.
   */
  createMentalModel(name, question, options = {}) {
    const q = question ?? options.question ?? options.sourceQuery
    if (!q) throw new TypeError('createMentalModel needs a question')
    let refresh = options.refresh
    if (refresh === undefined && options.refreshAfterConsolidation !== undefined) {
      refresh = options.refreshAfterConsolidation ? 'auto' : 'manual'
    }
    const body = { name, question: q }
    const fields = [
      ['id', options.id], ['folder', options.folder], ['tags', options.tags], ['refresh', refresh],
      ['max_tokens', options.maxTokens], ['budget', options.budget], ['fact_types', options.factTypes],
      ['body', options.body],
    ]
    for (const [key, value] of fields) if (value !== undefined && value !== null) body[key] = value
    return this.#feature('POST', '/mental-models', body)
  }
  /** Patch any create field (snake_case). `body` is a person's edit; `folder` moves it and changes its id. */
  updateMentalModel(id, fields) { return this.#feature('PATCH', `/mental-models/${seg(id)}`, fields) }
  /** Move to `folder` ('' for the top); resolves to the model under its new id. */
  moveMentalModel(id, folder) { return this.updateMentalModel(id, { folder }) }
  deleteMentalModel(id) { return this.#feature('DELETE', `/mental-models/${seg(id)}`) }
  /** Queue a refresh: `{operation_id, status, deduplicated}`. */
  refreshMentalModel(id, options = {}) {
    return this.#feature('POST', `/mental-models/${seg(id)}/refresh`, options.mode ? { mode: options.mode } : {})
  }
  acceptProposal(id) { return this.#feature('POST', `/mental-models/${seg(id)}/proposal/accept`, {}) }
  rejectProposal(id) { return this.#feature('POST', `/mental-models/${seg(id)}/proposal/reject`, {}) }
  /** The model's versions, or one version's `content`. */
  mentalModelHistory(id, version) {
    const tail = version === undefined ? '' : `/${seg(version)}`
    return this.#feature('GET', `/mental-models/${seg(id)}/history${tail}`)
  }
  /** Knowledge-page tree: `[{kind: 'folder'|'page', name, path, model?, children?}]`. */
  async mentalModelTree(options = {}) {
    return (await this.#feature('GET', '/mental-models-tree' + qs({ folder: options.folder })))?.roots ?? []
  }
  /** `[{path, content}]`, or with `{ markdown: true }` one markdown string. */
  async exportMentalModels(options = {}) {
    if (options.markdown) return this.#feature('GET', '/mental-models-export?format=markdown', undefined, { raw: true })
    return (await this.#feature('GET', '/mental-models-export'))?.files ?? []
  }

  // directives

  /** `{items, total}`; inactive ones too unless `activeOnly`. */
  directives(options = {}) {
    return this.#feature('GET', '/directives' + qs({
      tags: options.tags?.join(','), active_only: options.activeOnly ? 'true' : 'false',
    }))
  }
  createDirective(text, options = {}) {
    const body = { text }
    const fields = [['name', options.name], ['tags', options.tags], ['priority', options.priority], ['is_active', options.isActive]]
    for (const [key, value] of fields) if (value !== undefined && value !== null) body[key] = value
    return this.#feature('POST', '/directives', body)
  }
  /** Patch text, name, tags, priority, is_active. */
  updateDirective(id, fields) { return this.#feature('PATCH', `/directives/${seg(id)}`, fields) }
  deleteDirective(id) { return this.#feature('DELETE', `/directives/${seg(id)}`) }

  // operations

  /** `{bank_id, operations, total}`; each has `kind` (= `type`) and `status`. */
  operations(options = {}) {
    return this.#feature('GET', '/operations' + qs({
      status: options.status, type: options.type ?? options.kind, limit: options.limit, offset: options.offset,
    }))
  }
  operation(id) { return this.#feature('GET', `/operations/${seg(id)}`) }
  cancelOperation(id) { return this.#feature('DELETE', `/operations/${seg(id)}`) }
  /**
   * Poll until the operation is completed, failed or cancelled, and resolve
   * to it (a failure is returned, not thrown: read `error`). Rejects with a
   * GrimoireError after `timeoutMs`.
   */
  async waitOperation(id, options = {}) {
    const timeoutMs = options.timeoutMs ?? 120000
    const intervalMs = options.intervalMs ?? 500
    const deadline = Date.now() + timeoutMs
    for (;;) {
      const op = await this.operation(id)
      if (TERMINAL_STATUSES.includes(op?.status)) return op
      if (Date.now() >= deadline) throw new GrimoireError(0, `operation ${id} is still ${op?.status}`, '')
      await new Promise((resolve) => setTimeout(resolve, intervalMs))
    }
  }

  // webhooks

  /** `{items, total}`. */
  webhooks() { return this.#feature('GET', '/webhooks') }
  /** The secret (generated when not given) is in the response this once. */
  createWebhook(url, options = {}) {
    const body = { url, enabled: options.enabled ?? true }
    if (options.secret) body.secret = options.secret
    const events = options.events ?? options.eventTypes
    if (events?.length) body.events = events
    return this.#feature('POST', '/webhooks', body)
  }
  updateWebhook(id, fields) { return this.#feature('PATCH', `/webhooks/${seg(id)}`, fields) }
  deleteWebhook(id) { return this.#feature('DELETE', `/webhooks/${seg(id)}`) }
  async webhookDeliveries(id, options = {}) {
    return (await this.#feature('GET', `/webhooks/${seg(id)}/deliveries${qs({ limit: options.limit })}`))?.items ?? []
  }

  // templates

  /** The bank's configuration as a template manifest. */
  export() { return this.#feature('GET', '/export') }
  /** Apply a manifest, or `{ template: '<built-in id>' }`; additive. */
  importTemplate(manifestOrTemplate, options = {}) {
    const body = typeof manifestOrTemplate === 'string' ? { template: manifestOrTemplate }
      : manifestOrTemplate?.template ? { template: manifestOrTemplate.template }
        : { manifest: manifestOrTemplate?.manifest ?? manifestOrTemplate }
    return this.#feature('POST', '/import' + qs({ dry_run: options.dryRun ? 'true' : undefined }), body)
  }
}

/** The bank collection. */
export class Banks {
  constructor(client) { this.client = client }
  bank(id) { return new Bank(this.client, id) }
  async list() { return (await bankRequest(this.client, 'GET', '/api/banks'))?.banks ?? [] }
  create(id, fields = {}) { return bankRequest(this.client, 'POST', '/api/banks', { bank_id: id, ...fields }) }
  /** Built-in templates `[{id, name, description, manifest}]`; needs a signed-in user where accounts exist. */
  async templates() { return (await featureRequest(this.client, 'GET', '/api/bank-templates'))?.templates ?? [] }
  template(id) { return featureRequest(this.client, 'GET', `/api/bank-templates/${seg(id)}`) }
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
