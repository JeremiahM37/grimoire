import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ApiError, createClient, createGrimoireApi } from './api';

test('admin refusal never signals expired account or discards the editor session', async () => {
  let expired = 0;
  const api = createClient({ onSessionExpired: () => expired++, fetch: async () => Response.json({ detail: 'Admin token required' }, { status: 401, headers: { 'X-Grimoire-Gate': 'admin' } }) });
  await assert.rejects(api('/settings'), (error: unknown) => error instanceof ApiError && error.gate === 'admin' && error.status === 401);
  assert.equal(expired, 0);
});

test('account expiration signals the UI once and never retries a failed save', async () => {
  let expired = 0, calls = 0;
  const api = createClient({ onSessionExpired: () => expired++, fetch: async () => {
    calls++;
    return Response.json({ detail: 'Session expired' }, { status: 401 });
  }});
  await assert.rejects(api('/notes/test.md', { method: 'PUT', body: { body: 'unsaved draft' } }), ApiError);
  assert.equal(expired, 1);
  assert.equal(calls, 1);
});

test('note paths preserve folders while escaping URL metacharacters and Unicode', async () => {
  const api = createGrimoireApi({ fetch: async (url) => {
    assert.equal(url, '/api/notes/folder/%C3%A9%20%23%3F.md');
    return Response.json({ path: 'folder/é #?.md' });
  }});
  const note = await api.note('folder/é #?.md');
  assert.equal(note.path, 'folder/é #?.md');
});

test('save preserves explicit false values and request cancellation', async () => {
  const controller = new AbortController();
  const api = createClient({ fetch: async (_, init) => {
    assert.equal(init?.body, '{"private":false}');
    assert.equal(init?.signal, controller.signal);
    assert.equal(new Headers(init?.headers).get('X-Grimoire-Admin'), 'test-admin');
    return new Response(null, { status: 204 });
  }});
  assert.equal(await api('/notes/test.md', { method: 'PUT', body: { private: false }, signal: controller.signal, headers: { 'X-Grimoire-Admin': 'test-admin' } }), null);
});
