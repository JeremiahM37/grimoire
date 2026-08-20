/**
 * Type declarations for the Grimoire client.
 *
 * Hand-written rather than emitted: the package ships its source as the
 * published artifact, with no build step and no dependencies, so these types
 * are the contract rather than a by-product of one.
 */

/** One remembered fact. */
export interface Memory {
  id: string
  text: string
  path: string
  agent?: string
  task?: string
  session?: string
  category?: string
  stamp?: string
  expires?: string
  immutable?: boolean
  /** Set when a later fact replaced this one. */
  superseded_by?: string
  score: number
  /** Present when `explain` was requested. */
  scores?: {
    semantic: number
    keyword: number
    entity: number
    recency: number
  }
}

/** What one written fact did to what was already known. */
export interface Result {
  /** ADD stored it, UPDATE superseded another, DELETE retracted one, NOOP did nothing. */
  op: 'ADD' | 'UPDATE' | 'DELETE' | 'NOOP'
  id?: string
  text?: string
  /** The fact this superseded or retracted. */
  target?: string
  path?: string
  why?: string
}

export interface RememberResponse {
  path: string
  created: boolean
  entry: string
  op: Result['op']
  id: string
  results: Result[]
}

export interface AddOptions {
  topic?: string
  agent?: string
  task?: string
  /** Run or conversation id, so this run's learnings can be recalled together. */
  session?: string
  category?: string
  /** Time-to-live, e.g. "72h". */
  expires_in?: string
  /** Absolute RFC3339 expiry. */
  expires?: string
  /** Pin the fact: reconciliation may never supersede or retract it. */
  immutable?: boolean
  /** false stores the text verbatim, with no extraction and no reconciliation. */
  infer?: boolean
  /** Bounds what this write may supersede. Default: the whole vault. */
  scope?: 'vault' | 'topic' | 'session' | 'agent'
}

export interface SearchOptions {
  limit?: number
  agent?: string
  session?: string
  category?: string
  path?: string
  /** RFC3339 instant: what was believed then. */
  asOf?: string
  includeSuperseded?: boolean
  includeExpired?: boolean
  explain?: boolean
}

export interface EntryChanges {
  text?: string
  category?: string
  expires?: string
  expires_in?: string
  immutable?: boolean
}

export interface Scopes {
  agents: Record<string, number>
  sessions: Record<string, number>
  categories: Record<string, number>
  total: number
  live: number
}

export interface ClientOptions {
  token?: string
  /** Attribution written into every memory and retraction. */
  agent?: string
  fetch?: typeof fetch
}

export class GrimoireError extends Error {
  status: number
  detail: string
  url: string
}
export class NotFound extends GrimoireError {}
export class Unauthorized extends GrimoireError {}
export class VaultLocked extends GrimoireError {}

export function stored(result: Result): boolean
export function superseded(fact: Memory): boolean

export class Grimoire {
  constructor(url?: string, options?: ClientOptions)
  url: string
  token: string | null
  agent: string

  add(text: string, options?: AddOptions): Promise<Result>
  remember(text: string, options?: AddOptions): Promise<RememberResponse>
  addMany(items: Array<AddOptions & { text: string }>): Promise<Result[]>
  search(query?: string, options?: SearchOptions): Promise<Memory[]>
  getAll(options?: SearchOptions): Promise<Memory[]>
  history(asOf: string, options?: SearchOptions & { query?: string }): Promise<Memory[]>
  update(path: string, id: string, changes?: EntryChanges): Promise<{ path: string; entry: Memory }>
  delete(path: string, id: string, options?: { hard?: boolean }): Promise<Record<string, unknown>>
  scopes(): Promise<Scopes>
  export(filters?: { agent?: string; session?: string; category?: string }): Promise<{
    count: number
    exported: string
    entries: Memory[]
  }>
  consolidate(options?: { topic?: string; path?: string }): Promise<Record<string, unknown>>

  ask(question: string, options?: { limit?: number }): Promise<Record<string, unknown>>
  searchNotes(query: string, options?: { limit?: number }): Promise<Array<Record<string, unknown>>>
  readNote(path: string): Promise<Record<string, unknown>>
  writeNote(path: string, body: string, frontmatter?: Record<string, unknown>): Promise<Record<string, unknown>>
  briefing(options?: { memories?: number }): Promise<Record<string, unknown>>
  getFact(key: string, options?: { note?: string }): Promise<Array<Record<string, unknown>>>
  health(): Promise<Record<string, unknown>>
}

export default Grimoire
