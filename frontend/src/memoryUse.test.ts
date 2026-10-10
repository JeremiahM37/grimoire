import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ApiError } from './api';
import { createMemoryUseApi } from './memoryUseApi';
import {
  actionCodes, activeRules, benefit, evidenceMix, funnel, holdoutText, MIN_MATCHED, noteTarget, rateText, sectionHint, targetLabel, topLists,
  type AdherenceItem, type Benefit, type Card, type Causal, type Interval,
} from './memoryUseModel';

const iv = (k: number, n: number): Interval => ({ k, n, rate: n ? k / n : 0, lo: 0.1, hi: 0.5 });
const benefitOf = (matched: number): Benefit => ({ label: 'associated', influenced_actions: matched, matched_influenced_actions: matched, comparison_actions: 40, strata: 2,
  bad_rate_influenced: iv(1, matched), bad_rate_comparison_matched: 0.2, diff: -0.12, lo: -0.2, hi: -0.03 });
const card = (): Card => ({ target: 'note:Agent Memory/rule.md', kind: 'rule', exposures: 12, withheld: 2, uptake: iv(6, 10), influence: iv(4, 9),
  link_evidence: { tag: 2, fp: 5, check: 1, changed: 1 }, adherence_outcomes: { used: 4, ignored: 3, violated: 1 },
  linked_actions: { actions: 9, observed: 8, failed: 1, tests_failed: 0, tests_passed: 2, re_edited: 1, reverted: 0, thrash: 0, denied: 0, corrected: 0, bad: 2 },
  reminders: { reminders: 4, unchanged: 2, changed: 1, abandoned: 1, open: 0, ask: 0, ask_ran: 0, ask_not_run: 0, changed_rate: iv(1, 3) },
  benefit: benefitOf(MIN_MATCHED + 2) });
const holdoutOff: Causal = { label: '', status: 'holdout off', holdout_rate: 0, treated: 12, withheld: 0, min_per_arm: 10 };

test('funnel has the five stages and an n at each', () => {
  const stages = funnel(card(), holdoutOff);
  assert.deepEqual(stages.map(s => s.key), ['exposure', 'uptake', 'influence', 'outcome', 'benefit']);
  assert.equal(stages[0]!.n, 12);
  assert.deepEqual([stages[1]!.k, stages[1]!.n], [6, 10]);
  assert.deepEqual([stages[3]!.k, stages[3]!.n], [2, 8]); // bad of observed
});

test('benefit is associated, caused or insufficient data, never blurred', () => {
  assert.equal(benefit(benefitOf(MIN_MATCHED), holdoutOff).label, 'associated');
  assert.match(benefit(benefitOf(MIN_MATCHED), holdoutOff).text, /not proof/);
  const few = benefit(benefitOf(MIN_MATCHED - 1), holdoutOff);
  assert.equal(few.label, 'insufficient data');
  assert.match(few.text, /insufficient data/);
  const caused = benefit(benefitOf(2), { ...holdoutOff, status: 'estimated', label: 'caused', estimate: { Effect: -0.1, Lo: -0.18, Hi: -0.02, N: 80 } });
  assert.equal(caused.label, 'caused');
  assert.equal(caused.n, 80);
  assert.match(caused.text, /95% CI/);
  // An estimator that is not installed must never read as caused.
  assert.equal(benefit(benefitOf(30), { ...holdoutOff, status: 'estimator not installed' }).label, 'associated');
});

test('evidence mix always lists the four kinds in order', () => {
  assert.deepEqual(evidenceMix({ fp: 3 }).map(e => [e.key, e.count]), [['tag', 0], ['fp', 3], ['check', 0], ['changed', 0]]);
});

test('rates and labels read plainly', () => {
  assert.equal(rateText(iv(0, 0)), 'no data');
  assert.equal(rateText({ k: 6, n: 10, rate: 0.6, lo: 0.3, hi: 0.8 }), '60% (30%-80%, n=10)');
  assert.equal(targetLabel('note:a/b.md'), 'a/b.md');
  assert.equal(targetLabel('fact:0123456789abcdef'), 'fact 01234567');
  assert.equal(noteTarget('x.md'), 'note:x.md');
});

test('holdout status says off or the rate', () => {
  assert.equal(holdoutText(0, holdoutOff), 'off');
  assert.match(holdoutText(0.05, { ...holdoutOff, status: 'insufficient data', withheld: 3 }), /5\.0% .*insufficient data/);
});

test('top lists need exposures and rank by their own measure', () => {
  const item = (target: string, o: Partial<AdherenceItem>): AdherenceItem => ({ target, injected: 10, cited: 0, followed: 0, used: 0, violated: 0, ignored: 0, unknown: 0, contradicted: 0, pending: 0, rate: 0, used_rate: 0, fingerprint_coverage: 1, ...o });
  const t = topLists([item('a', { used: 8, used_rate: 0.8 }), item('b', { used: 3, used_rate: 0.3 }), item('c', { ignored: 9 }), item('d', { violated: 4 }), item('few', { injected: 1, violated: 1, used: 1, used_rate: 1 })]);
  assert.deepEqual(t.helpful.map(i => i.target), ['a', 'b']);
  assert.deepEqual(t.ignored.map(i => i.target), ['c']);
  assert.deepEqual(t.violated.map(i => i.target), ['d']);
});

test('only active and enforce checks are listed, most precise first', () => {
  const r = (id: string, status: string, precision: number) => ({ id, target: 't', rule_text: id, status, precision, ci95_low: 0, ci95_high: 1, labelled: 5, true_violations: 4, live_true: 0, live_false: 0, matches: 9 });
  assert.deepEqual(activeRules([r('s', 'suggestion', 1), r('a', 'active', 0.9), r('e', 'enforce', 0.97), r('d', 'disabled', 1)]).map(x => x.id), ['e', 'a']);
});

test('action codes carry outcomes only', () => {
  const base = { id: 1, seq: 1, tool: 'Edit', stage: 'early', ts: 0, failed: 0, tests_passed: -1, tests_failed: -1, re_edited: false, reverted: false, thrash: 0, denied: false, corrected: false };
  assert.deepEqual(actionCodes(base), ['ok']);
  assert.deepEqual(actionCodes({ ...base, failed: 1, tests_failed: 2, re_edited: true, corrected: true }), ['failed', '2 tests failed', 're-edited', 'corrected']);
  assert.deepEqual(actionCodes({ ...base, failed: -1 }), ['no signal']);
});

test('admin-only data shows a hint, not an error', () => {
  assert.equal(sectionHint(new ApiError(403, 'admin', 'administrators only')).kind, 'admin');
  assert.equal(sectionHint(new ApiError(401, '', 'sign in first')).kind, 'admin');
  assert.match(sectionHint(new ApiError(401, '', 'sign in first')).text, /Sign in/);
  assert.equal(sectionHint(new ApiError(404, '', 'no trace')).kind, 'none');
  assert.equal(sectionHint(new Error('boom')).kind, 'error');
});

test('api routes line up with the server', async () => {
  const seen: string[] = [];
  const request = (async (path: string) => { seen.push(path); return {}; }) as never;
  const api = createMemoryUseApi(request);
  await api.summary(7); await api.adherence(30); await api.rules(); await api.card('note:A B.md', 30);
  assert.deepEqual(seen, ['/memory/trace/summary?days=7', '/memory/adherence?days=30', '/memory/rules', '/memory/trace?target=note%3AA%20B.md&days=30']);
});
