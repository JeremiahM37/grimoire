import type { Grimoire, NotFound } from './index.js'

/** The server does not have this bank feature (yet); a missing bank is NotFound. */
export declare class NotAvailable extends NotFound {}

export type Budget = 'low' | 'mid' | 'high'
export type TagsMatch = 'any' | 'all' | 'any_strict' | 'all_strict'

export interface Disposition { skepticism: number; literalism: number; empathy: number }
export interface Directive { id?: string; text: string; tags?: string[] }

export interface BankProfile {
  bank_id: string
  name: string
  mission: string
  retain_mission: string
  disposition: Disposition
  tags: string[]
  directives: Directive[]
  config: Record<string, string>
  created?: string
  updated?: string
}

export interface BankSummary {
  bank_id: string
  name: string
  path: string
  facts: number
  documents: number
  updated?: string
}

export interface Turn { speaker: string; text: string; timestamp?: string }

export interface RetainItem {
  content: string | Turn[] | unknown
  timestamp?: string | null
  document_id?: string
  context?: string
  metadata?: Record<string, unknown>
  entities?: Array<{ text: string; type?: string }>
  tags?: string[]
  update_mode?: 'replace' | 'append'
}

export interface RetainOptions {
  items?: RetainItem[]
  documentId?: string
  timestamp?: string | Date
  context?: string
  tags?: string[]
  metadata?: Record<string, unknown>
  entities?: Array<{ text: string; type?: string }>
  updateMode?: 'replace' | 'append'
  documentTags?: string[]
  /** "chunks" stores each chunk as a fact with no model call. */
  mode?: 'concise' | 'verbatim' | 'chunks'
  async?: boolean
}

export interface BankFact {
  id: string
  text: string
  type: string
  entities?: string[]
  context?: string
  occurred_start?: string
  occurred_end?: string
  mentioned_at?: string
  document_id?: string
  chunk_id?: string
  tags: string[]
  metadata?: Record<string, string>
  authority: 'human' | 'agent'
  disputed_by?: string
  doc_removed?: boolean
  scores: { final: number; reranker: number | null; semantic: number | null; keyword: number | null }
}

export interface RecallOptions {
  budget?: Budget
  maxTokens?: number
  types?: string[]
  tags?: string[]
  tagsMatch?: TagsMatch
  queryTimestamp?: string | Date
  includeEntities?: boolean
  /** true, or the token budget for source chunks. */
  includeChunks?: boolean | number
  includeSourceFacts?: boolean
  trace?: boolean
}

export interface RecallResponse {
  results: BankFact[]
  entities?: Record<string, { entity_id: string; canonical_name: string }>
  chunks?: Record<string, { id: string; text: string; chunk_index: number; document_id: string; truncated: boolean }>
  source_facts?: Record<string, BankFact>
  trace?: Record<string, unknown>
}

export interface ReflectOptions {
  budget?: Budget
  maxTokens?: number
  context?: string
  responseSchema?: Record<string, unknown>
  types?: string[]
  tags?: string[]
  tagsMatch?: TagsMatch
  includeFacts?: boolean
  includeToolCalls?: boolean
}

export interface ReflectResponse {
  text: string
  based_on?: {
    memories?: BankFact[]
    observations?: Array<{ id: string; text: string }>
    mental_models?: Array<{ id: string; text: string }>
    directives?: Array<{ id: string; text: string }>
  }
  structured_output?: unknown
  usage?: { input_tokens: number; output_tokens: number; total_tokens: number }
  trace?: { tool_calls?: unknown[]; llm_calls?: unknown[] }
}

export interface MentalModel {
  id: string
  name: string
  source_query: string
  content: string
  tags: string[]
  max_tokens?: number
  trigger?: { refresh_after_consolidation?: boolean }
  last_refreshed_at?: string
  is_stale?: boolean
  authority?: 'human' | 'agent'
  pending_proposal?: { content: string; created_at: string; operation_id?: string }
}

export interface Operation {
  id: string
  operation_type: string
  status: 'pending' | 'processing' | 'completed' | 'failed' | 'cancelled'
  created_at: string
  updated_at?: string
  completed_at?: string
  error_message?: string
  progress?: { stage?: string; processed?: number; total?: number }
}

type Json = Record<string, unknown>

export declare class Bank {
  constructor(client: Grimoire, id: string)
  readonly client: Grimoire
  readonly id: string
  profile(): Promise<BankProfile>
  update(fields: Partial<Omit<BankProfile, 'bank_id' | 'created' | 'updated'>>): Promise<BankProfile>
  delete(): Promise<Json>
  retain(content?: RetainItem['content'], options?: RetainOptions): Promise<Json & { async_fallback?: boolean }>
  recall(query: string, options?: RecallOptions): Promise<RecallResponse>
  listMemories(options?: { q?: string; type?: string; documentId?: string; authority?: 'human' | 'agent'; limit?: number; offset?: number }): Promise<{ items: BankFact[]; total: number }>
  memory(id: string): Promise<BankFact>
  deleteMemory(id: string, options?: { force?: boolean }): Promise<Json>
  entities(options?: { q?: string; limit?: number }): Promise<Array<{ entity_id: string; canonical_name: string; mention_count: number; first_seen?: string; last_seen?: string }>>
  entity(idOrName: string, options?: { limit?: number }): Promise<Json>
  documents(options?: { limit?: number; offset?: number }): Promise<{ items: Json[]; total: number }>
  document(id: string): Promise<Json>
  deleteDocument(id: string, options?: { force?: boolean }): Promise<Json>
  chunk(id: string): Promise<Json>
  // Newer endpoints: reject with NotAvailable on a server without them.
  reflect(query: string, options?: ReflectOptions): Promise<ReflectResponse>
  observations(options?: { q?: string; limit?: number; offset?: number }): Promise<{ items: Json[]; total: number }>
  consolidate(): Promise<{ operation_id: string }>
  mentalModels(): Promise<{ items: MentalModel[]; total: number }>
  mentalModel(id: string): Promise<MentalModel>
  createMentalModel(name: string, sourceQuery: string, options?: { id?: string; tags?: string[]; maxTokens?: number; refreshAfterConsolidation?: boolean }): Promise<{ mental_model_id: string; operation_id?: string }>
  refreshMentalModel(id: string): Promise<{ operation_id: string; status: string }>
  acceptProposal(id: string): Promise<MentalModel>
  rejectProposal(id: string): Promise<MentalModel>
  deleteMentalModel(id: string): Promise<Json>
  operations(options?: { status?: string; type?: string; limit?: number; offset?: number }): Promise<{ operations: Operation[]; total: number }>
  operation(id: string): Promise<Operation>
  cancelOperation(id: string): Promise<Json>
  webhooks(): Promise<{ items: Json[]; total: number }>
  createWebhook(url: string, options?: { secret?: string; eventTypes?: string[]; enabled?: boolean }): Promise<Json>
  updateWebhook(id: string, fields: Json): Promise<Json>
  deleteWebhook(id: string): Promise<Json>
}

export declare class Banks {
  constructor(client: Grimoire)
  bank(id: string): Bank
  list(): Promise<BankSummary[]>
  create(id: string, fields?: Partial<Omit<BankProfile, 'bank_id'>>): Promise<BankProfile>
  templates(): Promise<Array<{ id: string; name: string; description?: string; manifest: Json }>>
}

export declare function bankRequest(client: Grimoire, method: string, path: string, body?: unknown): Promise<any>
export declare function featureRequest(client: Grimoire, method: string, path: string, body?: unknown): Promise<any>

export declare const MEMORY_HEADING: string
export declare function lastUserText(messages?: Array<{ role?: string; content?: unknown }>): string
export declare function formatMemories(results?: Array<Partial<BankFact>>, options?: { maxMemories?: number; now?: Date }): string
export declare function injectMemories<M extends { role?: string; content?: unknown }>(messages: M[], block: string): M[]

export interface MemoryOptions {
  inject?: boolean
  store?: boolean
  budget?: Budget
  maxTokens?: number
  types?: string[]
  recallTags?: string[]
  recallTagsMatch?: TagsMatch
  maxMemories?: number
  tags?: string[]
  /** Retained document id; a session accumulates by appending. */
  sessionId?: string
  /** Fixed recall query instead of the last user message. */
  query?: string
  /** Do not await the retain; errors go to onError and pendingErrors(). */
  background?: boolean
  onError?: (error: unknown) => void
}

/** Pass `grimoire: MemoryOptions` in a request to override for that call. */
export type WithMemory<C> = C & {
  pendingErrors(): unknown[]
  flush(): Promise<unknown>
  readonly memorySettings: MemoryOptions
}

export declare function withMemory<C extends { chat: { completions: { create: (...args: any[]) => any } } }>(
  client: C, bank: Bank, options?: MemoryOptions): WithMemory<C>
