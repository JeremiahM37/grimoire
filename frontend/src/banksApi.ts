// Every call the Banks panel makes goes through this module, so the routes it
// uses can be lined up with the server in one place. The routes and shapes are
// the ones docs/MEMORY_BANKS.md lists.
//
// A server from before reflect, observations, mental models, directives,
// operations and templates existed answers those routes with the mux's plain
// 404; the calls marked "optional" resolve to UNAVAILABLE then, so the panel
// can say "not on this server" instead of showing an error.
import { ApiError } from './api';
import type { JsonValue } from './types';
import { builtinTemplates, type BankTemplate, type Manifest } from './bankTemplates';

export type Request = <T = JsonValue>(path: string, init?: { method?: string; body?: JsonValue | FormData; signal?: AbortSignal }) => Promise<T>;

export interface Disposition { skepticism: number; literalism: number; empathy: number }
export interface Directive { id: string; name?: string; text: string; tags?: string[]; priority?: number; inactive?: boolean }
export interface DirectiveInput { text?: string; name?: string; tags?: string[]; priority?: number; is_active?: boolean }
export interface BankSummary { bank_id: string; name: string; path: string; facts: number; documents: number; updated?: string }
export interface BankProfile {
  bank_id: string; name: string; mission: string; retain_mission: string; disposition: Disposition;
  tags: string[]; directives: Directive[]; config: Record<string, string>; created?: string; updated?: string;
}
export interface BankStats {
  bank_id: string; facts: number; facts_by_type: Record<string, number>; human_facts: number; observations: number; documents: number;
  entities: number; mental_models: number; pending_consolidation: number; operations_by_status: Record<string, number>;
  consolidation: string; model_available: boolean;
}
export interface Scores { final: number; reranker: number | null; semantic: number | null; keyword: number | null }
export interface Fact {
  id: string; text: string; type: string; entities?: string[]; context?: string; occurred_start?: string; occurred_end?: string;
  mentioned_at?: string; document_id?: string; chunk_id?: string; tags: string[]; metadata?: Record<string, string>;
  authority: string; disputed_by?: string; doc_removed?: boolean; source_fact_ids?: string[]; proof_count?: number; scores?: Scores;
}
export interface ArmHit { id: string; rank: number; score: number }
export interface Trace {
  thinking_budget: number; temporal_window?: { start: string; end: string }; arms: Record<string, ArmHit[]>;
  ranks: Record<string, Record<string, number>>; fused_candidates: number; reranked: boolean; tokens_used: number;
  disputes?: Record<string, string>; timings_ms: Record<string, number>; bank_facts: number; keyword_terms?: string[];
}
export interface RecallResponse {
  results: Fact[]; entities?: Record<string, { entity_id: string; canonical_name: string }>;
  chunks?: Record<string, ChunkOut>; trace?: Trace;
}
export interface ChunkOut { id: string; text: string; chunk_index: number; document_id: string; truncated: boolean }
export interface EntitySummary { entity_id: string; canonical_name: string; mention_count: number; first_seen?: string; last_seen?: string }
export interface EntityDetail extends EntitySummary { related: EntitySummary[]; facts: Fact[] }
export interface DocumentSummary { document_id: string; path: string; timestamp?: string; context?: string; tags: string[]; chunks: number; chars: number; facts: number; updated?: string }
export interface DocumentDetail {
  document_id: string; timestamp?: string; context?: string; tags: string[]; metadata?: Record<string, string>; chunk_size: number;
  chunk_hashes?: string[]; content?: string; path: string; facts_path: string; facts: Fact[];
}
export interface RetainItem { content: string; document_id?: string; context?: string; tags?: string[]; timestamp?: string }
export interface RetainDocument { document_id: string; chunks: number; chunks_extracted: number; chunks_reused: number; facts: number; facts_added: number; human_facts_kept: number; unchanged?: boolean }
export interface RetainResponse { bank_id: string; items_count: number; async: boolean; mode?: string; documents?: RetainDocument[]; operation_id?: string; operation_ids?: string[] }
export interface RecallParams { query: string; budget?: string; max_tokens?: number; types?: string[]; tags?: string[]; trace?: boolean }

export interface ReflectParams { query: string; budget?: string; max_tokens?: number; fact_types?: string[]; tags?: string[]; context?: string; trace?: boolean }
export interface ReflectToolCall { tool: string; input?: JsonValue; output?: JsonValue; duration_ms?: number; iteration?: number; forced?: boolean; error?: string }
export interface ReflectResponse {
  text: string; mode: 'llm' | 'extractive' | string;
  based_on: { memories: Fact[]; observations: Fact[]; mental_models: { id: string; name: string; text: string }[]; directives: Directive[] };
  structured_output?: JsonValue; structured_output_error?: string;
  usage?: { input_tokens?: number; output_tokens?: number; total_tokens?: number };
  iterations?: number; directives_checked?: boolean;
  trace?: { levels?: string[]; tool_calls?: ReflectToolCall[]; llm_calls?: { scope: string; duration_ms?: number; error?: string }[]; rejected_citations?: string[] } | null;
}
export interface Evidence { fact_id: string; quote?: string }
export interface Observation {
  id: string; text: string; authority: string; source_fact_ids: string[]; proof_count: number; evidence?: Evidence[]; tags: string[];
  occurred_start?: string; occurred_end?: string; mentioned_at?: string; updated_at?: string; challenges?: string;
  of?: string; superseded_at?: string; deleted?: boolean;
}
export interface Proposal { content: string; based_on: string[]; created_at: string; base_version: number }
export interface MentalModel {
  id: string; bank_id: string; name: string; question: string; folder: string; path: string; tags: string[]; refresh: string;
  max_tokens: number; budget: string; fact_types?: string[]; body?: string; version: number; last_refreshed?: string; based_on: string[];
  authority: string; is_stale: boolean; stale_reason?: string; pending_proposal?: Proposal; updated?: string;
}
export interface ModelInput { id?: string; name?: string; question?: string; folder?: string; tags?: string[]; refresh?: string; max_tokens?: number; budget?: string; fact_types?: string[]; body?: string }
export interface ModelNode { kind: 'folder' | 'page'; name: string; path: string; model?: MentalModel; children?: ModelNode[] }
export interface Operation {
  id: string; bank_id: string; kind: string; type: string; status: string; payload?: JsonValue; result?: JsonValue; error?: string;
  attempts: number; cancel_requested?: boolean; progress?: string; created_at: string; started_at?: string; finished_at?: string; updated_at?: string;
}
export interface ImportResult {
  bank_id: string; bank_created: boolean; config_applied: string[]; mental_models_created: string[]; mental_models_updated: string[];
  directives_created: string[]; directives_updated: string[]; operation_ids: string[]; dry_run: boolean;
}

/** The value an optional call resolves to when this server lacks the route. */
export const UNAVAILABLE = Symbol('unavailable');
export type Maybe<T> = T | typeof UNAVAILABLE;

/** A 404 (or 405) from a route this panel assumes means the server lacks it. */
export function isMissingRoute(error: unknown): boolean {
  // An unknown route answers the mux's plain-text 404 (or a 405 for a method
  // it does not route), so the client falls back to the status text. A
  // missing bank or model answers JSON ({"detail": "not found"}), which is a
  // real error rather than a missing feature.
  if (!(error instanceof ApiError)) return false;
  if (error.status === 405) return true;
  return error.status === 404 && (error.message === 'Not Found' || error.message === 'Request failed (404)');
}

/** The server needs a language model for this and has none configured. */
export const isModelRequired = (error: unknown) => error instanceof ApiError && error.status === 409 && error.code === 'model_required';

async function optional<T>(call: Promise<T>): Promise<Maybe<T>> {
  try {
    return await call;
  } catch (error) {
    if (isMissingRoute(error)) return UNAVAILABLE;
    throw error;
  }
}

const enc = encodeURIComponent;
const qs = (params: Record<string, string | number | boolean | undefined>) => {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '' && v !== false) q.set(k, String(v));
  return q.size ? `?${q}` : '';
};

export function createBanksApi(request: Request) {
  const b = (bank: string) => `/banks/${enc(bank)}`;
  // Model ids may hold folders ("people/dana"); the whole id is one path segment.
  const m = (bank: string, id: string) => `${b(bank)}/mental-models/${enc(id)}`;
  const body = (value: unknown) => value as JsonValue;
  return {
    list: () => request<{ banks: BankSummary[] }>('/banks').then(r => r.banks || []),
    create: (profile: Partial<Omit<BankProfile, 'directives'>> & { bank_id: string; directives?: { text: string; name?: string }[] }) =>
      request<BankProfile>('/banks', { method: 'POST', body: body(profile) }),
    profile: (bank: string) => request<BankProfile>(b(bank)),
    update: (bank: string, patch: Partial<BankProfile>) => request<BankProfile>(b(bank), { method: 'PATCH', body: body(patch) }),
    remove: (bank: string) => request(b(bank), { method: 'DELETE' }),
    stats: (bank: string) => optional(request<BankStats>(`${b(bank)}/stats`)),
    retain: (bank: string, items: RetainItem[], opts: { mode?: string; async?: boolean } = {}) =>
      request<RetainResponse>(`${b(bank)}/memories`, { method: 'POST', body: body({ items, mode: opts.mode || undefined, async: opts.async || undefined }) }),
    memories: (bank: string, params: { q?: string; type?: string; authority?: string; document_id?: string; limit?: number; offset?: number }) =>
      request<{ items: Fact[]; total: number }>(`${b(bank)}/memories${qs(params)}`),
    deleteMemory: (bank: string, id: string, force = false) => request(`${b(bank)}/memories/${enc(id)}${force ? '?force=true' : ''}`, { method: 'DELETE' }),
    recall: (bank: string, params: RecallParams) => request<RecallResponse>(`${b(bank)}/memories/recall`, { method: 'POST', body: body(params) }),
    entities: (bank: string, q = '', limit = 100) => request<{ items: EntitySummary[] }>(`${b(bank)}/entities${qs({ q, limit })}`).then(r => r.items || []),
    entity: (bank: string, id: string) => request<EntityDetail>(`${b(bank)}/entities/${enc(id)}`),
    documents: (bank: string, limit = 100, offset = 0) => request<{ items: DocumentSummary[]; total: number }>(`${b(bank)}/documents${qs({ limit, offset })}`),
    document: (bank: string, id: string) => request<DocumentDetail>(`${b(bank)}/documents/${enc(id)}`),
    deleteDocument: (bank: string, id: string, force = false) => request<{ human_facts_kept?: number }>(`${b(bank)}/documents/${enc(id)}${force ? '?force=true' : ''}`, { method: 'DELETE' }),
    chunk: (bank: string, id: string) => request<ChunkOut>(`${b(bank)}/chunks/${enc(id)}`),

    // ---- reflect ----
    /** Reflect; the trace (tool calls) comes back only when asked for. */
    reflect: (bank: string, { trace, ...params }: ReflectParams) =>
      optional(request<ReflectResponse>(`${b(bank)}/reflect`, { method: 'POST', body: body({ ...params, include: trace ? { facts: {}, tool_calls: {} } : { facts: {} } }) })),

    // ---- observations and consolidation ----
    observations: (bank: string, params: { q?: string; authority?: string; include_history?: boolean; limit?: number; offset?: number } = {}) =>
      optional(request<{ items: Observation[]; total: number; history?: Observation[] }>(`${b(bank)}/observations${qs({ ...params, include_history: params.include_history ? 1 : undefined })}`)),
    observation: (bank: string, id: string) => request<{ observation: Observation; history: Observation[] }>(`${b(bank)}/observations/${enc(id)}`),
    deleteObservation: (bank: string, id: string, force = false) => request(`${b(bank)}/observations/${enc(id)}${force ? '?force=true' : ''}`, { method: 'DELETE' }),
    /** Queue a consolidation. 409 model_required when no model is configured. */
    consolidate: (bank: string) => optional(request<{ operation_id: string; deduplicated: boolean }>(`${b(bank)}/consolidate`, { method: 'POST', body: {} })),

    // ---- mental models ----
    mentalModels: (bank: string, params: { folder?: string; detail?: 'full' } = {}) => optional(request<{ items: MentalModel[]; total: number }>(`${b(bank)}/mental-models${qs(params)}`)),
    modelTree: (bank: string) => optional(request<{ roots: ModelNode[] }>(`${b(bank)}/mental-models-tree`)),
    mentalModel: (bank: string, id: string) => request<MentalModel>(m(bank, id)),
    /** Create a model. operation_id is null when the body was given or the server has no model to write it. */
    createMentalModel: (bank: string, model: ModelInput & { name: string; question: string }) =>
      optional(request<{ mental_model: MentalModel; mental_model_id: string; operation_id: string | null }>(`${b(bank)}/mental-models`, { method: 'POST', body: body(model) })),
    /** Edit a model. A `body` is a person's edit; a new `folder` moves it and changes its id. */
    updateMentalModel: (bank: string, id: string, patch: ModelInput) => request<MentalModel>(m(bank, id), { method: 'PATCH', body: body(patch) }),
    refreshMentalModel: (bank: string, id: string) => request<{ operation_id: string; status: string; deduplicated: boolean }>(`${m(bank, id)}/refresh`, { method: 'POST', body: {} }),
    acceptProposal: (bank: string, id: string) => request<MentalModel>(`${m(bank, id)}/proposal/accept`, { method: 'POST', body: {} }),
    rejectProposal: (bank: string, id: string) => request<{ rejected: string }>(`${m(bank, id)}/proposal/reject`, { method: 'POST', body: {} }),
    deleteMentalModel: (bank: string, id: string) => request(m(bank, id), { method: 'DELETE' }),

    // ---- directives ----
    directives: (bank: string, activeOnly = false) => optional(request<{ items: Directive[]; total: number }>(`${b(bank)}/directives${qs({ active_only: activeOnly ? undefined : 'false' })}`)),
    createDirective: (bank: string, d: DirectiveInput & { text: string }) => request<Directive>(`${b(bank)}/directives`, { method: 'POST', body: body(d) }),
    updateDirective: (bank: string, id: string, d: DirectiveInput) => request<Directive>(`${b(bank)}/directives/${enc(id)}`, { method: 'PATCH', body: body(d) }),
    deleteDirective: (bank: string, id: string) => request(`${b(bank)}/directives/${enc(id)}`, { method: 'DELETE' }),

    // ---- operations ----
    operations: (bank: string, params: { status?: string; type?: string; limit?: number; offset?: number } = {}) =>
      optional(request<{ bank_id: string; operations: Operation[]; total: number }>(`${b(bank)}/operations${qs(params)}`)),
    operation: (bank: string, id: string) => request<Operation>(`${b(bank)}/operations/${enc(id)}`),
    cancelOperation: (bank: string, id: string) => request<{ success: boolean; operation_id: string; status: string }>(`${b(bank)}/operations/${enc(id)}`, { method: 'DELETE' }),

    // ---- templates ----
    /** The server's templates; the built-in copies (marked builtin) when it has none. */
    templates: async (): Promise<(BankTemplate & { builtin?: boolean })[]> => {
      const got = await optional(request<{ templates: BankTemplate[] }>('/bank-templates'));
      return got === UNAVAILABLE || !got.templates?.length ? builtinTemplates.map(t => ({ ...t, builtin: true })) : got.templates;
    },
    exportManifest: (bank: string) => request<Manifest>(`${b(bank)}/export`),
    importManifest: (bank: string, manifest: Manifest, dryRun = false) =>
      optional(request<ImportResult>(`${b(bank)}/import${dryRun ? '?dry_run=1' : ''}`, { method: 'POST', body: body({ manifest }) })),
  };
}

export type BanksApi = ReturnType<typeof createBanksApi>;

/** The template's manifest with the name and mission a person typed in place of the template's. */
export function withOverrides(manifest: Manifest, overrides: { name?: string; mission?: string }): Manifest {
  const bank = { ...(manifest.bank || {}) };
  if (overrides.name) bank.name = overrides.name;
  if (overrides.mission) bank.mission = overrides.mission;
  return { ...manifest, version: manifest.version || '1', bank };
}

/** Create a bank, from a template when one is chosen.
 *
 * The bank is created first (so an existing id is refused, 409), then the
 * template is imported: the server sets the profile and creates the mental
 * models and directives. On a server without the import route the template's
 * profile fields and directives go in the create call instead, and its mental
 * models are skipped. */
export async function createFromTemplate(api: BanksApi, bankId: string, template: (BankTemplate & { builtin?: boolean }) | undefined, overrides: { name?: string; mission?: string }) {
  const manifest = withOverrides(template?.manifest || {}, overrides);
  const bank = manifest.bank || {};
  if (template && !template.builtin) {
    const profile = await api.create({ bank_id: bankId, ...(overrides.name ? { name: overrides.name } : {}) });
    const imported = await api.importManifest(bankId, manifest);
    if (imported !== UNAVAILABLE) return { profile, imported, models: imported.mental_models_created.length };
  }
  const profile: Parameters<BanksApi['create']>[0] = { bank_id: bankId };
  if (bank.name) profile.name = bank.name;
  if (bank.mission) profile.mission = bank.mission;
  if (bank.retain_mission) profile.retain_mission = bank.retain_mission;
  if (bank.disposition) profile.disposition = bank.disposition;
  if (manifest.directives?.length) profile.directives = manifest.directives.map(d => ({ text: d.text, ...(d.name ? { name: d.name } : {}) }));
  if (bank.config && Object.keys(bank.config).length) profile.config = bank.config;
  if (template && !template.builtin) {
    // The bank already exists (the import route is missing): set the profile.
    const { bank_id: _, ...patch } = profile;
    return { profile: await api.update(bankId, patch as Partial<BankProfile>), imported: undefined, models: 0 };
  }
  return { profile: await api.create(profile), imported: undefined, models: 0 };
}
