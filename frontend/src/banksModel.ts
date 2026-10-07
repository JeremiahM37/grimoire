// Pure helpers for the Banks panel: no React, no network, so they are unit
// tested directly (banks.test.ts).

export { builtinTemplates, type BankTemplate, type Manifest, type TemplateMentalModel } from './bankTemplates';

/** The per-bank settings the server accepts, with their allowed values. */
export const CONFIG_KEYS: { key: string; values: string[]; label: string }[] = [
  { key: 'retain_extraction_mode', values: ['concise', 'verbatim', 'chunks'], label: 'Extraction mode' },
  { key: 'retain_extract_causal', values: ['true', 'false'], label: 'Extract causes' },
  { key: 'enable_text_search', values: ['true', 'false'], label: 'Keyword arm' },
  { key: 'enable_graph', values: ['true', 'false'], label: 'Graph arm' },
  { key: 'enable_temporal', values: ['true', 'false'], label: 'Temporal arm' },
  { key: 'enable_reranking', values: ['true', 'false'], label: 'Reranking' },
  { key: 'consolidation', values: ['auto', 'manual', 'off'], label: 'Consolidation' },
];

/** Free-form per-bank settings (text inputs rather than choices). */
export const CONFIG_TEXT_KEYS: { key: string; label: string; placeholder: string }[] = [
  { key: 'observations_mission', label: 'Observations mission', placeholder: 'What consolidation should track' },
  { key: 'consolidation_batch_size', label: 'Facts per consolidation call', placeholder: '8' },
  { key: 'reflect_max_tokens', label: 'Reflect answer length (tokens)', placeholder: 'e.g. 1024' },
  { key: 'mcp_tools', label: 'MCP tools allowed', placeholder: 'e.g. bank_recall,reflect (empty = all)' },
];

/** Why a mental model is stale, in words. */
export function staleText(reason?: string): string {
  if (reason === 'never_refreshed') return 'never refreshed';
  if (reason === 'memories_changed') return 'memories changed since the last refresh';
  return reason || '';
}

/** Bank ids the server accepts. */
export const validBankId = (id: string) => /^[a-z0-9][a-z0-9._:-]{0,63}$/.test(id);

/** Same as go/internal/bank ChunkID: `~` and `_` are escaped so `_` can separate the parts. */
export function chunkId(bank: string, doc: string, index: number): string {
  const esc = (s: string) => s.replace(/~/g, '~7E').replace(/_/g, '~5F');
  return `${esc(bank)}_${esc(doc)}_${index}`;
}

export interface FactLike { tags?: string[]; occurred_start?: string; occurred_end?: string; mentioned_at?: string }

/** The date a fact is filed under: when it happened, else when it was said. */
export const factDate = (f: FactLike) => (f.occurred_start || f.mentioned_at || '').slice(0, 10);

/** Client-side filters the server has no parameter for. */
export function filterFacts<T extends FactLike>(facts: T[], opts: { tag?: string; from?: string; to?: string }): T[] {
  const tag = opts.tag?.trim().toLowerCase();
  return facts.filter(f => {
    if (tag && !(f.tags || []).some(t => t.toLowerCase() === tag)) return false;
    const day = factDate(f);
    if (opts.from && (!day || day < opts.from)) return false;
    if (opts.to && (!day || day > opts.to)) return false;
    return true;
  });
}

/** Rows for the per-arm rank table: one per result, one column per arm. */
export function rankRows(resultIds: string[], ranks: Record<string, Record<string, number>> | undefined, arms: string[]) {
  return resultIds.map(id => ({ id, ranks: arms.map(arm => ranks?.[id]?.[arm] ?? null) }));
}

/** Arms in a stable order, known ones first. */
export function armNames(trace?: { arms?: Record<string, unknown>; ranks?: Record<string, Record<string, number>> }): string[] {
  const known = ['semantic', 'keyword', 'graph', 'temporal'];
  const seen = new Set<string>(Object.keys(trace?.arms || {}));
  for (const r of Object.values(trace?.ranks || {})) for (const arm of Object.keys(r)) seen.add(arm);
  return [...known.filter(a => seen.has(a)), ...[...seen].filter(a => !known.includes(a)).sort()];
}

export interface GraphPoint { id: string; label: string; x: number; y: number; r: number; center: boolean }

/** A compact radial layout: the chosen entity in the middle, companions round it,
 * or, with no centre, the top entities on a ring. Sizes follow mentions. */
export function radialLayout(nodes: { id: string; label: string; weight: number }[], centerId: string | undefined, size = 260): GraphPoint[] {
  const max = Math.max(1, ...nodes.map(n => n.weight));
  const radius = (w: number) => 5 + 9 * Math.sqrt(w / max);
  const mid = size / 2;
  const ring = nodes.filter(n => n.id !== centerId);
  const points: GraphPoint[] = [];
  const center = nodes.find(n => n.id === centerId);
  if (center) points.push({ id: center.id, label: center.label, x: mid, y: mid, r: radius(center.weight) + 3, center: true });
  ring.forEach((n, i) => {
    const angle = (i / Math.max(1, ring.length)) * Math.PI * 2 - Math.PI / 2;
    const dist = center ? size * 0.36 : size * 0.38;
    points.push({ id: n.id, label: n.label, x: mid + Math.cos(angle) * dist, y: mid + Math.sin(angle) * dist, r: radius(n.weight), center: false });
  });
  return points;
}

/** A person-readable name for an operation kind. */
export function opKindLabel(kind: string): string {
  return ({ retain: 'Retain', consolidation: 'Consolidation', refresh_mental_model: 'Refresh mental model' } as Record<string, string>)[kind] || kind;
}

/** Operation statuses, in the order the server moves through them. */
export const OP_STATUSES = ['queued', 'running', 'completed', 'failed', 'cancelled'];

export const isTerminal = (status: string) => ['completed', 'failed', 'cancelled'].includes(status);

export const fmtScore = (v: number | null | undefined) => (v === null || v === undefined ? '—' : v.toFixed(3));

export const parseTags = (s: string) => s.split(',').map(t => t.trim()).filter(Boolean);

/** The events a webhook can listen to; "*" is all of them. */
export const WEBHOOK_EVENTS = ['retain.completed', 'consolidation.completed', 'reflect.completed', '*'];

/** A webhook address must be http(s); the server decides whether a private one is allowed. */
export function validWebhookUrl(s: string): boolean {
  try { const u = new URL(s.trim()); return u.protocol === 'http:' || u.protocol === 'https:'; } catch { return false; }
}

/** Events as a short label: none sent means the server's defaults. */
export const eventsLabel = (events: string[] | undefined) => (events && events.length ? events.join(', ') : 'default events');

/** One delivery as a short status line: "delivered (200)", "failed after 3 attempts: timeout". */
export function deliveryText(d: { status: string; attempts: number; last_response_status?: number; last_error?: string }): string {
  if (d.status === 'delivered') return d.last_response_status ? `delivered (${d.last_response_status})` : 'delivered';
  if (d.status === 'failed') return `failed after ${d.attempts} attempt${d.attempts === 1 ? '' : 's'}${d.last_error ? ': ' + d.last_error : ''}`;
  return d.attempts ? `retrying, attempt ${d.attempts}${d.last_error ? ': ' + d.last_error : ''}` : 'pending';
}
