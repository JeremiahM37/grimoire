import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createDisputesApi } from './disputesApi';
import { authorityText, contestText, MAX_MERGE_TEXT, resolveBody, type Dispute, type DisputeSide } from './disputesModel';

const side = (id: string, text: string, authority: string): DisputeSide => ({ id, path: 'memory/ops.md', text, authority, evidence: [] });
const one = (): Dispute => ({ id: 'orig', path: 'memory/ops.md', disputed: side('orig', 'runs on 6432', 'human'), challengers: [side('c1', 'runs on 5432', 'agent')] });
const two = (): Dispute => ({ ...one(), challengers: [side('c1', 'runs on 5432', 'agent'), side('c2', 'runs on 5433', 'agent')] });

test('keep sends the id, the note and nothing else', () => {
  const r = resolveBody(one(), 'keep');
  assert.deepEqual(r, { ok: true, body: { id: 'orig', path: 'memory/ops.md', resolution: 'keep' } });
});

test('accept takes the only challenger without asking, and asks when there are several', () => {
  const single = resolveBody(one(), 'accept_challenger');
  assert.deepEqual(single.ok && single.body, { id: 'orig', path: 'memory/ops.md', resolution: 'accept_challenger', challenger: 'c1' });
  const asked = resolveBody(two(), 'accept_challenger');
  assert.equal(asked.ok, false);
  const picked = resolveBody(two(), 'accept_challenger', { challenger: 'c2' });
  assert.equal(picked.ok && picked.body.challenger, 'c2');
  const stranger = resolveBody(two(), 'accept_challenger', { challenger: 'nobody' });
  assert.equal(stranger.ok, false);
});

test('merge needs text that is not blank and not too long', () => {
  assert.equal(resolveBody(one(), 'merge', { text: '   ' }).ok, false);
  const ok = resolveBody(one(), 'merge', { text: '  runs on 6432 behind pgbouncer  ' });
  assert.equal(ok.ok && ok.body.text, 'runs on 6432 behind pgbouncer');
  assert.equal(resolveBody(one(), 'merge', { text: 'x'.repeat(MAX_MERGE_TEXT + 1) }).ok, false);
});

test('wording says whose fact is whose', () => {
  assert.equal(authorityText('human'), 'a person');
  assert.equal(authorityText('agent'), 'an agent');
  assert.equal(contestText(one()), '1 fact contests this');
  assert.equal(contestText(two()), '2 facts contest this');
});

test('the api posts resolutions to the resolve route', async () => {
  const calls: { path: string; method?: string; body?: unknown }[] = [];
  const api = createDisputesApi(async (path, init) => {
    calls.push({ path, method: init?.method, body: init?.body });
    return {} as never;
  });
  await api.resolve({ id: 'orig', resolution: 'keep' });
  assert.deepEqual(calls, [{ path: '/memory/disputes/resolve', method: 'POST', body: { id: 'orig', resolution: 'keep' } }]);
  await api.list();
  assert.equal(calls[1]?.path, '/memory/disputes');
});
