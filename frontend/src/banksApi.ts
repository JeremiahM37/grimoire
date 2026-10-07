// Every call the Banks panel makes goes through this module, so the routes it
// assumes can be lined up with the server in one place.
//
// The server today has: banks CRUD, retain, recall, memories, entities,
// documents and chunks. Reflect, observations, consolidation, mental models,
// operations and bank templates are coded against their planned shapes; a 404
// from one of those means "this server does not have it yet", which the
// panel shows as such instead of as an error.
import { ApiError } from './api';
import type { JsonValue } from './types';
import { builtinTemplates, type BankTemplate } from './bankTemplates';

export type Request = <T = JsonValue>(path: string, init?: { method?: string; body?: JsonValue | FormData; signal?: AbortSignal }) => Promise<T>;

export interface Disposition { skepticism: number; literalism: number; empathy: number }
export interface Directive { id?: string; text: string; tags?: string[] }
export interface BankSummary { bank_id: string; name: string; path: string; facts: number; documents: number; updated?: string }
export interface BankProfile {
  bank_id: string; name: string; mission: string; retain_mission: string; disposition: Disposition;
  tags: string[]; directives: Directive[]; config: Record<string, string>; created?: string; updated?: string;
}
export interface Scores { final: number; reranker: number | null; semantic: number | null; keyword: number | null }
export interface Fact {
  id: string; text: string; type: string; entities?: string[]; context?: string; occurred_start?: string; occurred_end?: string;
  mentioned_at?: string; document_id?: string; chunk_id?: string; tags: string[]; metadata?: Record<string, string>;
  authority: string; disputed_by?: string; doc_removed?: boolean; scores?: Scores;
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
export interface RecallParams { query: string; budget?: string; max_tokens?: number; types?: string[]; tags?: string[]; trace?: boolean }

// ---- not yet on the server (planned shapes) ----
export interface ReflectResponse {
  text: string;
  based_on?: { memories?: Fact[]; observations?: { id: string; text: string }[]; mental_models?: { id: string; text: string }[]; directives?: { id: string; text?: string; name?: string; content?: string }[] };
  usage?: { input_tokens?: number; output_tokens?: number; total_tokens?: number };
  trace?: { tool_calls?: { tool: string; input?: JsonValue; output?: JsonValue; duration_ms?: number; iteration?: number }[]; llm_calls?: { scope: string; duration_ms?: number }[] };
}
export interface Observation { id: string; text: string; evidence?: string[]; proof_count?: number; tags?: string[]; authority?: string; disputed_by?: string; updated?: string }
export interface MentalModel {
  id: string; name: string; source_query: string; content?: string; tags?: string[]; max_tokens?: number;
  trigger?: { refresh_after_consolidation?: boolean }; last_refreshed_at?: string; is_stale?: boolean; authority?: string;
  pending_proposal?: { content: string; created_at?: string; operation_id?: string } | null;
}
export interface Operation {
  id: string; operation_type: string; status: string; created_at?: string; updated_at?: string; completed_at?: string;
  error_message?: string; progress?: { stage?: string; processed?: number; total?: number };
}

/** The value a not-yet endpoint resolves to when this server lacks it. */
export const UNAVAILABLE = Symbol('unavailable');
export type Maybe<T> = T | typeof UNAVAILABLE;

/** A 404 (or 405) from a route this panel assumes means the server lacks it. */
export function isMissingRoute(error: unknown): boolean {
  // An unknown route answers the mux's plain-text 404 (or a 405 for a method
  // it does not route), so the client falls back to the status text. A
  // missing bank or model answers JSON ({"detail": "no such bank"} /
  // "not found"), which is a real error rather than a missing feature.
  if (!(error instanceof ApiError)) return false;
  if (error.status === 405) return true;
  return error.status === 404 && (error.message === 'Not Found' || error.message === 'Request failed (404)');
}

async function optional<T>(call: Promise<T>): Promise<Maybe<T>> {
  try {
    return await call;
  } catch (error) {
    if (isMissingRoute(error)) return UNAVAILABLE;
    throw error;
  }
}

const enc = encodeURIComponent;
const qs = (params: Record<string, string | number | undefined>) => {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') q.set(k, String(v));
  return q.size ? `?${q}` : '';
};

export function createBanksApi(request: Request) {
  const b = (bank: string) => `/banks/${enc(bank)}`;
  const body = (value: unknown) => value as JsonValue;
  return {
    // ---- existing routes ----
    list: () => request<{ banks: BankSummary[] }>('/banks').then(r => r.banks || []),
    create: (profile: Partial<BankProfile> & { bank_id: string }) => request<BankProfile>('/banks', { method: 'POST', body: body(profile) }),
    profile: (bank: string) => request<BankProfile>(b(bank)),
    update: (bank: string, patch: Partial<BankProfile>) => request<BankProfile>(b(bank), { method: 'PATCH', body: body(patch) }),
    remove: (bank: string) => request(b(bank), { method: 'DELETE' }),
    retain: (bank: string, items: RetainItem[], mode?: string) => request<{ documents: { document_id: string; facts: number }[] }>(`${b(bank)}/memories`, { method: 'POST', body: body({ items, mode: mode || undefined }) }),
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

    // ---- planned routes: each resolves to UNAVAILABLE on a 404 ----
    reflect: (bank: string, params: { query: string; budget?: string; max_tokens?: number; types?: string[]; tags?: string[] }) =>
      optional(request<ReflectResponse>(`${b(bank)}/reflect`, { method: 'POST', body: body({ ...params, include: { facts: {}, tool_calls: {} } }) })),
    observations: (bank: string, params: { q?: string; limit?: number; offset?: number } = {}) =>
      optional(request<{ items: Observation[]; total: number }>(`${b(bank)}/observations${qs(params)}`)),
    consolidate: (bank: string) => optional(request<{ operation_id: string }>(`${b(bank)}/consolidate`, { method: 'POST', body: {} })),
    mentalModels: (bank: string) => optional(request<{ items: MentalModel[]; total: number }>(`${b(bank)}/mental-models`)),
    mentalModel: (bank: string, id: string) => optional(request<MentalModel>(`${b(bank)}/mental-models/${enc(id)}`)),
    createMentalModel: (bank: string, model: { id?: string; name: string; source_query: string; tags?: string[]; max_tokens?: number; trigger?: { refresh_after_consolidation?: boolean } }) =>
      optional(request<{ mental_model_id: string; operation_id?: string }>(`${b(bank)}/mental-models`, { method: 'POST', body: body(model) })),
    refreshMentalModel: (bank: string, id: string) => optional(request<{ operation_id: string; status: string }>(`${b(bank)}/mental-models/${enc(id)}/refresh`, { method: 'POST', body: {} })),
    acceptProposal: (bank: string, id: string) => optional(request<MentalModel>(`${b(bank)}/mental-models/${enc(id)}/proposal/accept`, { method: 'POST', body: {} })),
    rejectProposal: (bank: string, id: string) => optional(request<MentalModel>(`${b(bank)}/mental-models/${enc(id)}/proposal/reject`, { method: 'POST', body: {} })),
    deleteMentalModel: (bank: string, id: string) => optional(request(`${b(bank)}/mental-models/${enc(id)}`, { method: 'DELETE' })),
    operations: (bank: string, params: { status?: string; limit?: number; offset?: number } = {}) =>
      optional(request<{ operations: Operation[]; total: number }>(`${b(bank)}/operations${qs(params)}`)),
    cancelOperation: (bank: string, id: string) => optional(request(`${b(bank)}/operations/${enc(id)}`, { method: 'DELETE' })),
    /** Server templates, or the built-in ones when the server has none. */
    templates: async (): Promise<BankTemplate[]> => {
      const got = await optional(request<{ templates: BankTemplate[] }>('/bank-templates'));
      return got === UNAVAILABLE || !got.templates?.length ? builtinTemplates : got.templates;
    },
  };
}

export type BanksApi = ReturnType<typeof createBanksApi>;

/** Create a bank from a template: the profile in one call, then its mental
 * models best-effort (skipped when the server has no mental models yet). */
export async function createFromTemplate(api: BanksApi, bankId: string, template: BankTemplate | undefined, overrides: { name?: string; mission?: string }) {
  const bank = template?.manifest.bank || {};
  const profile: Partial<BankProfile> & { bank_id: string } = { bank_id: bankId };
  if (overrides.name || bank.name) profile.name = overrides.name || bank.name;
  const mission = overrides.mission || bank.mission;
  if (mission) profile.mission = mission;
  if (bank.retain_mission) profile.retain_mission = bank.retain_mission;
  if (bank.disposition) profile.disposition = bank.disposition;
  if (bank.directives?.length) profile.directives = bank.directives.map(d => ({ text: d.text }));
  if (bank.config && Object.keys(bank.config).length) profile.config = bank.config;
  const created = await api.create(profile);
  let models = 0;
  for (const model of template?.manifest.mental_models || []) {
    const r = await api.createMentalModel(bankId, model).catch(() => UNAVAILABLE);
    if (r === UNAVAILABLE) break;
    models++;
  }
  return { profile: created, models };
}
