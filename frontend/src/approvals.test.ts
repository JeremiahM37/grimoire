import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ApiError } from './api';
import { ADMIN_HINT, approvalsAllowed, approvalsErrorText } from './ApprovalsPanel';
import { type SourceAction, clip, isDecided, outcomeText, paramLines, pendingCount, plainAction, safeLink, sortHistory, stateLabel } from './approvals';

const base = (over: Partial<SourceAction>): SourceAction => ({
  id: 'a1', source: 'c1', kind: 'gmail', action: 'create_draft', state: 'pending',
  created: '2026-10-10T09:00:00Z', agent: 'claude-code', params: {}, ...over,
});

test('the headline says what the action does, in plain words, per kind', () => {
  assert.equal(
    plainAction(base({ params: { to: 'ana@example.com', subject: 'Invoice', body: 'hi' } })),
    'Draft an email to ana@example.com: subject "Invoice"');
  assert.equal(
    plainAction(base({ kind: 'gmail', action: 'send_message', params: { to: 'ana@example.com', subject: 'Hi' } })),
    'Send an email to ana@example.com: subject "Hi"');
  assert.equal(
    plainAction(base({ kind: 'slack', action: 'post_message', params: { channel: 'C024BE91L', text: 'Deploy done' } })),
    'Post to channel C024BE91L: Deploy done');
  assert.equal(
    plainAction(base({ kind: 'github', action: 'create_issue', params: { title: 'Flaky test' } }), { id: 'c1', name: 'owner/repo', kind: 'github' }),
    'Create GitHub issue in owner/repo: Flaky test');
  assert.equal(
    plainAction(base({ kind: 'github', action: 'comment', params: { number: '42', body: 'Thanks' } }), { id: 'c1', name: 'mine', kind: 'github' }),
    'Comment on GitHub #42 in mine: Thanks');
  assert.equal(
    plainAction(base({ kind: 'gcal', action: 'create_event', params: { summary: 'Dentist', start: '2026-10-12T09:00:00Z', end: '2026-10-12T10:00:00Z' } })),
    'Create calendar event "Dentist" from 2026-10-12T09:00:00Z to 2026-10-12T10:00:00Z');
  assert.equal(
    plainAction(base({ kind: 'gdrive', action: 'create_doc', params: { title: 'Plan' } })),
    'Create Google Doc "Plan"');
});

test("a GitHub headline names the connector's repository, not the connector", () => {
  assert.equal(
    plainAction(base({ kind: 'github', action: 'create_issue', params: { title: 'T' } }), { id: 'c1', name: 'my repo connector', kind: 'github', config: { repo: 'owner/repo' } }),
    'Create GitHub issue in owner/repo: T');
});

test('an unknown action falls back to the server summary, then to its name', () => {
  assert.equal(plainAction(base({ kind: 'linear', action: 'zap', summary: 'Linear zap on mine' })), 'Linear zap on mine');
  assert.equal(plainAction(base({ kind: 'linear', action: 'zap' })), 'Linear: zap');
});

test('long agent-written text is clipped in the headline but never dropped from parameters', () => {
  const long = 'x'.repeat(300);
  const headline = plainAction(base({ kind: 'slack', action: 'post_message', params: { channel: 'C1', text: long } }));
  assert.ok(headline.endsWith('…'));
  assert.ok(headline.length < 160);
  assert.equal(clip('  spaced\n\n out  '), 'spaced out');
  assert.equal(clip(''), '(empty)');
});

test('parameters are plain text lines, in the order a person reads them', () => {
  assert.deepEqual(
    paramLines({ body: 'b', zeta: 'z', to: 'a@x', subject: 's', alpha: 'a' }),
    ['to: a@x', 'subject: s', 'body: b', 'alpha: a', 'zeta: z']);
  // Markup stays literal text: it is shown, never interpreted.
  assert.deepEqual(paramLines({ body: '<img src=x onerror=alert(1)>' }), ['body: <img src=x onerror=alert(1)>']);
  assert.deepEqual(paramLines(undefined), []);
});

test('counts and states: only waiting actions are pending, and decided ones are history', () => {
  const rows = [
    base({ id: '1', state: 'pending' }),
    base({ id: '2', state: 'executed', decided: '2026-10-10T10:00:00Z', result: { message: 'draft created' } }),
    base({ id: '3', state: 'failed', decided: '2026-10-10T11:00:00Z', error: 'no credential' }),
    base({ id: '4', state: 'denied', decided: '2026-10-10T08:00:00Z', note: 'not now' }),
    base({ id: '5', state: 'running' }),
  ];
  assert.equal(pendingCount(rows), 1);
  assert.deepEqual(sortHistory(rows).map(r => r.id), ['3', '2', '4']);
  assert.ok(isDecided(rows[1]!) && !isDecided(rows[0]!) && !isDecided(rows[4]!));
  assert.equal(stateLabel('executed'), 'Done');
  assert.equal(stateLabel('failed'), 'Failed');
  assert.equal(stateLabel('denied'), 'Denied');
});

test('outcomes say what happened: the confirmation, the error or the note', () => {
  assert.equal(outcomeText(base({ state: 'executed', result: { message: 'issue created' } })), 'issue created');
  assert.equal(outcomeText(base({ state: 'failed', error: 'HTTP 403' })), 'Failed: HTTP 403');
  assert.equal(outcomeText(base({ state: 'denied', note: 'wrong channel' })), 'Denied: wrong channel');
  assert.equal(outcomeText(base({ state: 'denied' })), 'Denied.');
});

test('only http(s) links from provider results are offered as links', () => {
  assert.equal(safeLink('https://github.com/o/r/issues/1'), 'https://github.com/o/r/issues/1');
  assert.equal(safeLink('javascript:alert(1)'), undefined);
  assert.equal(safeLink('data:text/html,hi'), undefined);
  assert.equal(safeLink(undefined), undefined);
});

test('an admin refusal reads as the admin-credential hint, not as a session problem', () => {
  assert.equal(approvalsErrorText(new ApiError(401, 'admin', 'needs token')), ADMIN_HINT);
  assert.equal(approvalsErrorText(new ApiError(403, '', 'administrators only')), ADMIN_HINT);
  assert.equal(approvalsErrorText(new ApiError(502, '', 'provider down')), 'provider down');
  assert.equal(approvalsErrorText(new Error('boom')), 'boom');
});

test('the badge and panel are offered to administrators and to single-user instances only', () => {
  assert.equal(approvalsAllowed(undefined), false);
  assert.equal(approvalsAllowed({ admin: true, multi_user: true }), true);
  assert.equal(approvalsAllowed({ admin: false, multi_user: true }), false);
  assert.equal(approvalsAllowed({ admin: false, multi_user: false }), true);
});
