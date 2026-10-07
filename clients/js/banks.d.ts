import type { Grimoire, GrimoireError, NotFound } from './index.js'

/** The server does not have this bank feature (it predates it); a missing bank is NotFound. */
export declare class NotAvailable extends NotFound {}

/** 409 `{code: 'model_required'}`: the call needs a language model and the server has none. */
export declare class ModelRequired extends GrimoireError {
  readonly code: 'model_required'
}

export declare const TERMINAL_STATUSES: readonly ['completed', 'failed', 'cancelled']

export type Budget = 'low' | 'mid' | 'high'
export type TagsMatch = 'any' | 'all' | 'any_strict' | 'all_strict'

export interface Disposition { skepticism: number; literalism: number; empathy: number }
export interface Directive { id?: string; name?: string; text: string; tags?: string[]; priority?: number; inactive?: boolean }

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
  factTypes?: string[]
  /** Alias of factTypes. */
  types?: string[]
  tags?: string[]
  tagsMatch?: TagsMatch
  tagGroups?: Record<string, unknown>[]
  applyAllDirectives?: boolean
  excludeMentalModels?: boolean
  excludeMentalModelIds?: string[]
  queryTimestamp?: string | Date
  includeFacts?: boolean
  /** Return the trace (tool calls). */
  includeToolCalls?: boolean
  trace?: boolean
}

/** A memory or observation as reflect reports it. */
export interface ReflectMemory {
  id: string
  text: string
  type?: string
  authority: 'human' | 'agent'
  disputed_by?: string
  occurred?: string
  mentioned_at?: string
  context?: string
  tags?: string[]
  proof_count?: number
  source_fact_ids?: string[]
}

export interface ReflectResponse {
  text: string
  mode: 'llm' | 'extractive'
  based_on: {
    memories: ReflectMemory[]
    observations: ReflectMemory[]
    mental_models: Array<{ id: string; name: string; text: string }>
    directives: Directive[]
  }
  structured_output?: unknown
  structured_output_error?: string
  usage: { input_tokens: number; output_tokens: number; total_tokens: number }
  iterations: number
  directives_checked: boolean
  /** Only with includeToolCalls or trace. */
  trace: null | {
    levels?: string[]
    tool_calls: Array<{ tool: string; input: Record<string, unknown>; output?: unknown; duration_ms: number; iteration: number; forced?: boolean; error?: string }>
    llm_calls: Array<{ scope: string; duration_ms: number; error?: string }>
    rejected_citations?: string[]
  }
}

export interface Observation {
  id: string
  text: string
  authority: 'human' | 'agent'
  source_fact_ids: string[]
  proof_count: number
  evidence?: Array<{ fact_id: string; quote?: string }>
  tags: string[]
  occurred_start?: string
  occurred_end?: string
  mentioned_at?: string
  updated_at?: string
  /** The person's observation this one disputes. */
  challenges?: string
  // history entries only
  of?: string
  superseded_at?: string
  deleted?: boolean
}

export interface Proposal { content: string; based_on: string[]; created_at: string; base_version: number }

export interface MentalModel {
  id: string
  bank_id: string
  name: string
  question: string
  folder: string
  path: string
  tags: string[]
  refresh: 'auto' | 'manual'
  max_tokens: number
  budget: Budget
  fact_types?: string[]
  /** The answer; on lists only with detail. */
  body?: string
  version: number
  last_refreshed?: string
  based_on: string[]
  authority: 'human' | 'agent'
  is_stale: boolean
  stale_reason?: 'never_refreshed' | 'memories_changed'
  pending_proposal?: Proposal
  updated?: string
}

export interface MentalModelSpec {
  id?: string
  name?: string
  question?: string
  folder?: string
  tags?: string[]
  refresh?: 'auto' | 'manual'
  max_tokens?: number
  budget?: Budget
  fact_types?: string[]
  /** A person's text: later refreshes file a proposal instead of overwriting it. */
  body?: string
}

export interface ModelNode { kind: 'folder' | 'page'; name: string; path: string; model?: MentalModel; children?: ModelNode[] }

export type OperationStatus = 'queued' | 'running' | 'completed' | 'failed' | 'cancelled'

export interface Operation {
  id: string
  bank_id: string
  kind: 'retain' | 'consolidation' | 'refresh_mental_model' | string
  /** Same as kind. */
  type: string
  status: OperationStatus
  payload?: unknown
  result?: unknown
  error?: string
  attempts: number
  cancel_requested?: boolean
  progress?: string
  created_at: string
  started_at?: string
  finished_at?: string
  updated_at?: string
}

export interface Webhook { id: string; bank_id: string; url: string; events: string[]; has_secret: boolean; enabled: boolean; created_at: string; updated_at: string; secret?: string }

export interface TemplateManifest {
  version: string
  bank?: { name?: string; mission?: string; retain_mission?: string; disposition?: Disposition; tags?: string[]; config?: Record<string, string> }
  mental_models?: Array<{ id: string; name: string; question: string; tags?: string[]; refresh?: string; max_tokens?: number; budget?: string; fact_types?: string[] }>
  directives?: Array<{ name?: string; text: string; tags?: string[]; priority?: number; inactive?: boolean }>
}

export interface BankTemplate { id: string; name: string; description: string; manifest: TemplateManifest }

export interface ImportResult {
  bank_id: string
  bank_created: boolean
  config_applied: string[]
  mental_models_created: string[]
  mental_models_updated: string[]
  directives_created: string[]
  directives_updated: string[]
  operation_ids: string[]
  dry_run: boolean
}

type Json = Record<string, unknown>

export declare class Bank {
  constructor(client: Grimoire, id: string)
  readonly client: Grimoire
  readonly id: string
  profile(): Promise<BankProfile>
  update(fields: Partial<Omit<BankProfile, 'bank_id' | 'created' | 'updated'>>): Promise<BankProfile>
  delete(): Promise<Json>
  retain(content?: RetainItem['content'], options?: RetainOptions): Promise<Json & { async?: boolean; operation_id?: string; operation_ids?: string[]; async_fallback?: boolean }>
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
  // Coding-agent surfaces.
  context(options?: { maxChars?: number; source?: 'startup' | 'resume' | 'clear' | 'compact' }): Promise<{ context: string; chars: number; limit: number; included: number; dropped: number }>
  index(query?: string, options?: { types?: string[]; since?: string; limit?: number; offset?: number }): Promise<{ items: Json[]; total: number }>
  timeline(anchor: string, options?: { before?: number; after?: number }): Promise<{ entries: Json[]; anchor_ref?: string }>
  fileMemory(path: string, options?: { limit?: number }): Promise<{ path: string; items: Json[] }>
  duplicates(options?: { minScore?: number; type?: 'fact' | 'observation'; limit?: number }): Promise<{ candidates: Json[] }>
  mergeDuplicates(keep: string, merge: string): Promise<Json>
  getEntries(ids: string[]): Promise<{ items: Json[]; missing: string[] }>
  writeDigest(sessionId: string, turns: { speaker: string; text: string; timestamp?: string }[], options?: { activity?: Json; useModel?: boolean }): Promise<Json>
  sessions(options?: { limit?: number }): Promise<{ items: Json[]; total: number }>
  // Reasoning endpoints: reject with NotAvailable on a server without them,
  // and with ModelRequired where a model is needed and none is configured.
  stats(): Promise<Json & { model_available: boolean }>
  reflect(query: string, options?: ReflectOptions): Promise<ReflectResponse>
  observations(options?: { q?: string; authority?: 'human' | 'agent'; tags?: string[]; tagsMatch?: TagsMatch; includeHistory?: boolean; limit?: number; offset?: number }): Promise<{ items: Observation[]; total: number; history?: Observation[] }>
  observation(id: string): Promise<{ observation: Observation; history: Observation[] }>
  updateObservation(id: string, text: string): Promise<{ observation: Observation }>
  deleteObservation(id: string, options?: { force?: boolean }): Promise<Json>
  clearObservations(): Promise<{ retired: number }>
  consolidate(): Promise<{ operation_id: string; deduplicated: boolean }>
  mentalModels(options?: { tags?: string[]; tagsMatch?: TagsMatch; folder?: string; detail?: boolean }): Promise<{ items: MentalModel[]; total: number }>
  mentalModel(id: string): Promise<MentalModel>
  createMentalModel(name: string, question?: string, options?: { id?: string; folder?: string; tags?: string[]; refresh?: 'auto' | 'manual'; maxTokens?: number; budget?: Budget; factTypes?: string[]; body?: string; question?: string; sourceQuery?: string; refreshAfterConsolidation?: boolean }): Promise<{ mental_model: MentalModel; mental_model_id: string; operation_id: string | null }>
  updateMentalModel(id: string, fields: MentalModelSpec): Promise<MentalModel>
  moveMentalModel(id: string, folder: string): Promise<MentalModel>
  deleteMentalModel(id: string): Promise<Json>
  refreshMentalModel(id: string, options?: { mode?: 'full' | 'delta' }): Promise<{ operation_id: string; status: 'queued'; deduplicated: boolean }>
  acceptProposal(id: string): Promise<MentalModel>
  rejectProposal(id: string): Promise<{ rejected: string }>
  mentalModelHistory(id: string, version?: string | number): Promise<Json>
  mentalModelTree(options?: { folder?: string }): Promise<ModelNode[]>
  exportMentalModels(options?: { markdown?: false }): Promise<Array<{ path: string; content: string }>>
  exportMentalModels(options: { markdown: true }): Promise<string>
  directives(options?: { tags?: string[]; activeOnly?: boolean }): Promise<{ items: Directive[]; total: number }>
  createDirective(text: string, options?: { name?: string; tags?: string[]; priority?: number; isActive?: boolean }): Promise<Directive>
  updateDirective(id: string, fields: { text?: string; name?: string; tags?: string[]; priority?: number; is_active?: boolean }): Promise<Directive>
  deleteDirective(id: string): Promise<Json>
  operations(options?: { status?: OperationStatus; type?: string; kind?: string; limit?: number; offset?: number }): Promise<{ bank_id: string; operations: Operation[]; total: number }>
  operation(id: string): Promise<Operation>
  cancelOperation(id: string): Promise<{ success: boolean; operation_id: string; status: OperationStatus }>
  waitOperation(id: string, options?: { timeoutMs?: number; intervalMs?: number }): Promise<Operation>
  webhooks(): Promise<{ items: Webhook[]; total: number }>
  createWebhook(url: string, options?: { secret?: string; events?: string[]; eventTypes?: string[]; enabled?: boolean }): Promise<Webhook>
  updateWebhook(id: string, fields: Json): Promise<Webhook>
  deleteWebhook(id: string): Promise<Json>
  webhookDeliveries(id: string, options?: { limit?: number }): Promise<Json[]>
  export(): Promise<TemplateManifest>
  importTemplate(manifestOrTemplate: string | TemplateManifest | { manifest: TemplateManifest } | { template: string }, options?: { dryRun?: boolean }): Promise<ImportResult>
}

export declare class Banks {
  constructor(client: Grimoire)
  bank(id: string): Bank
  list(): Promise<BankSummary[]>
  create(id: string, fields?: Partial<Omit<BankProfile, 'bank_id'>>): Promise<BankProfile>
  templates(): Promise<BankTemplate[]>
  template(id: string): Promise<BankTemplate>
}

export declare function bankRequest(client: Grimoire, method: string, path: string, body?: unknown, options?: { raw?: boolean }): Promise<any>
export declare function featureRequest(client: Grimoire, method: string, path: string, body?: unknown, options?: { raw?: boolean }): Promise<any>

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
