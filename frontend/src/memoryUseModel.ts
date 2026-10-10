// What the Memory use panel shows, and the wording rules for it. Everything
// here is pure so it can be tested without a browser. The routes and shapes
// are the ones docs/MEMORY_TRACE.md, MEMORY_ADHERENCE.md and MEMORY_RULES.md
// list.
import { ApiError } from './api';

export interface Interval { k: number; n: number; rate: number; lo: number; hi: number }
export interface Benefit {
  label: string; influenced_actions: number; matched_influenced_actions: number; comparison_actions: number; strata: number;
  bad_rate_influenced: Interval; bad_rate_comparison_matched: number; diff: number; lo: number; hi: number; note?: string;
}
export interface Reminders {
  reminders: number; unchanged: number; changed: number; abandoned: number; open: number;
  ask: number; ask_ran: number; ask_not_run: number; changed_rate: Interval;
}
export interface LinkedActions {
  actions: number; observed: number; failed: number; tests_failed: number; tests_passed: number;
  re_edited: number; reverted: number; thrash: number; denied: number; corrected: number; bad: number;
}
export interface ActionRow {
  id: number; seq: number; tool: string; stage: string; ts: number; failed: number; tests_passed: number; tests_failed: number;
  re_edited: boolean; reverted: boolean; thrash: number; denied: boolean; corrected: boolean;
}
export interface Card {
  target: string; kind?: string; exposures: number; withheld: number; uptake: Interval; influence: Interval;
  link_evidence: Record<string, number>; adherence_outcomes: Record<string, number>; linked_actions: LinkedActions;
  reminders: Reminders; benefit: Benefit; recent_actions?: ActionRow[];
}
export interface Causal {
  label: string; status: string; holdout_rate: number; treated: number; withheld: number; min_per_arm: number;
  estimator?: string; estimate?: { Method?: string; Effect: number; Lo: number; Hi: number; N?: number }; note?: string;
}
export interface CardResponse { days: number; card: Card; causal: Causal; labels?: Record<string, string> }
export interface KindRow {
  kind: string; memories: number; exposures: number; withheld: number; uptake: Interval; influence: Interval;
  reminders: Reminders; benefit: Benefit; causal: Causal;
}
export interface SummaryResponse { days: number; holdout_rate: number; kinds: KindRow[] }
export interface AdherenceItem {
  target: string; injected: number; cited: number; followed: number; used: number; violated: number; ignored: number;
  unknown: number; contradicted: number; pending: number; rate: number; used_rate: number; fingerprint_coverage: number;
}
export interface AdherenceResponse {
  days: number; overall: Omit<AdherenceItem, 'target'>; memories: AdherenceItem[];
}
export interface RuleRow {
  id: string; target: string; rule_text: string; spec?: { shape?: string }; status: string; precision: number;
  ci95_low: number; ci95_high: number; labelled: number; true_violations: number; live_true: number; live_false: number;
  matches: number; user_state?: string;
}
export interface RulesResponse { checks: RuleRow[] }

/** Fewer matched influenced actions than this is too few to read a difference. */
export const MIN_MATCHED = 10;

export const pct = (rate: number | undefined, digits = 0): string => `${((rate ?? 0) * 100).toFixed(digits)}%`;
/** "42% (31-53%, n=57)" */
export function rateText(iv: Interval | undefined): string {
  if (!iv || !iv.n) return 'no data';
  return `${pct(iv.rate)} (${pct(iv.lo)}-${pct(iv.hi)}, n=${iv.n})`;
}
export const points = (v: number): string => `${v > 0 ? '+' : ''}${(v * 100).toFixed(1)} points`;

export const noteTarget = (path: string): string => `note:${path}`;
export function targetLabel(target: string): string {
  if (target.startsWith('note:')) return target.slice(5);
  if (target.startsWith('fact:')) return `fact ${target.slice(5, 13)}`;
  return target;
}
export const targetNotePath = (target: string): string | undefined => (target.startsWith('note:') ? target.slice(5) : undefined);

export interface Stage { key: string; label: string; k?: number; n: number; rate?: number; hint: string }
/** The funnel, one row per stage, each with the n it was measured on. */
export function funnel(card: Card, causal?: Causal): Stage[] {
  const l = card.linked_actions;
  const observed = l?.observed ?? 0, bad = l?.bad ?? 0;
  const b = card.benefit;
  return [
    { key: 'exposure', label: 'Exposure', n: card.exposures, hint: `shown ${card.exposures} time${card.exposures === 1 ? '' : 's'}${card.withheld ? `, withheld ${card.withheld}` : ''}` },
    { key: 'uptake', label: 'Uptake', k: card.uptake.k, n: card.uptake.n, rate: card.uptake.rate, hint: 'the agent cited it or acted on its fingerprint' },
    { key: 'influence', label: 'Influence', k: card.influence.k, n: card.influence.n, rate: card.influence.rate, hint: 'linked to a concrete action' },
    { key: 'outcome', label: 'Outcome', k: bad, n: observed, rate: observed ? bad / observed : undefined, hint: 'linked actions with a bad outcome, of those observed' },
    { key: 'benefit', label: 'Benefit', n: benefit(b, causal).n, hint: benefit(b, causal).text },
  ];
}

export type BenefitView = { label: 'associated' | 'caused' | 'insufficient data'; text: string; n: number };
/** Benefit wording: "caused" only from an estimated holdout; "associated" from the matched comparison; else "insufficient data". */
export function benefit(b: Benefit | undefined, causal?: Causal): BenefitView {
  if (causal?.status === 'estimated' && causal.estimate) {
    const e = causal.estimate;
    return { label: 'caused', n: e.N ?? causal.treated + causal.withheld, text: `caused: ${points(e.Effect)} in bad outcomes (95% CI ${points(e.Lo)} to ${points(e.Hi)}), randomised holdout` };
  }
  const matched = b?.matched_influenced_actions ?? 0;
  if (!b || matched < MIN_MATCHED) {
    return { label: 'insufficient data', n: matched, text: `insufficient data: ${matched} matched influenced action${matched === 1 ? '' : 's'}, need ${MIN_MATCHED}` };
  }
  return { label: 'associated', n: matched, text: `associated: ${points(b.diff)} in bad outcomes (95% CI ${points(b.lo)} to ${points(b.hi)}), a comparison, not proof` };
}

const EVIDENCE: [string, string][] = [['tag', 'tag cited'], ['fp', 'fingerprint'], ['check', 'check met'], ['changed', 'reminder changed the call']];
export function evidenceMix(ev: Record<string, number> | undefined): { key: string; label: string; count: number }[] {
  return EVIDENCE.map(([key, label]) => ({ key, label, count: ev?.[key] ?? 0 }));
}
export const evidenceTotal = (ev: Record<string, number> | undefined): number => evidenceMix(ev).reduce((s, e) => s + e.count, 0);

const OUTCOMES = ['followed', 'used', 'cited', 'ignored', 'violated', 'contradicted', 'unknown', 'pending'] as const;
export function outcomeMix(o: Record<string, number> | undefined): { key: string; count: number }[] {
  return OUTCOMES.map(key => ({ key, count: o?.[key] ?? 0 }));
}

/** Outcome codes of one linked action, as short words. */
export function actionCodes(a: ActionRow): string[] {
  const out: string[] = [];
  if (a.failed === 1) out.push('failed'); else if (a.failed === 0) out.push('ok');
  if (a.tests_failed > 0) out.push(`${a.tests_failed} test${a.tests_failed === 1 ? '' : 's'} failed`);
  else if (a.tests_passed > 0) out.push(`${a.tests_passed} tests passed`);
  if (a.re_edited) out.push('re-edited');
  if (a.reverted) out.push('reverted');
  if (a.thrash >= 3) out.push(`repeated x${a.thrash}`);
  if (a.denied) out.push('denied');
  if (a.corrected) out.push('corrected');
  return out.length ? out : ['no signal'];
}

const MIN_LIST = 3;
export interface TopLists { helpful: AdherenceItem[]; ignored: AdherenceItem[]; violated: AdherenceItem[] }
/** Top memories, each list needing a few exposures so one lucky hit does not top it. */
export function topLists(items: AdherenceItem[] | undefined, size = 5): TopLists {
  const seen = (items ?? []).filter(i => i.injected >= MIN_LIST);
  const take = (rows: AdherenceItem[]) => rows.slice(0, size);
  return {
    helpful: take(seen.filter(i => i.used + i.followed + i.cited > 0).sort((a, b) => b.used_rate - a.used_rate || b.injected - a.injected)),
    ignored: take(seen.filter(i => i.ignored > 0).sort((a, b) => b.ignored / b.injected - a.ignored / a.injected || b.ignored - a.ignored)),
    violated: take(seen.filter(i => i.violated > 0).sort((a, b) => b.violated - a.violated || b.injected - a.injected)),
  };
}

export const activeRules = (rows: RuleRow[] | undefined): RuleRow[] =>
  (rows ?? []).filter(r => r.status === 'active' || r.status === 'enforce').sort((a, b) => b.precision - a.precision);

export function holdoutText(rate: number | undefined, causal?: Causal): string {
  if (!rate) return 'off';
  const base = `${pct(rate, rate && rate < 0.1 ? 1 : 0)} of eligible items withheld`;
  return causal && causal.status && causal.status !== 'holdout off' ? `${base} (${causal.status})` : base;
}

/** A hint for a failed section, instead of a bare error. */
export function sectionHint(error: unknown): { kind: 'admin' | 'none' | 'error'; text: string } {
  if (error instanceof ApiError) {
    if (error.status === 401 || error.status === 403 || error.gate === 'admin') {
      return { kind: 'admin', text: error.status === 401 && error.gate !== 'admin' ? 'Sign in as an administrator to see this.' : 'Administrators only. Sign in with an administrator account or the console admin credential.' };
    }
    if (error.status === 404) return { kind: 'none', text: 'No trace for this memory in the window.' };
    if (error.status === 503) return { kind: 'error', text: 'The adherence store is not available on this server.' };
  }
  return { kind: 'error', text: error instanceof Error ? error.message : String(error) };
}
