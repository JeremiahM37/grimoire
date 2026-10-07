import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ApiError, createClient } from './api';
import { createBanksApi, createFromTemplate, isMissingRoute, isModelRequired, UNAVAILABLE, withOverrides } from './banksApi';
import { armNames, builtinTemplates, chunkId, CONFIG_KEYS, CONFIG_TEXT_KEYS, filterFacts, opKindLabel, radialLayout, rankRows, staleText, validBankId } from './banksModel';

test('chunk ids match the server encoding', () => {
  assert.equal(chunkId('t1', 'd1', 0), 't1_d1_0');
  assert.equal(chunkId('coding-agent:repo', 'git:abc', 3), 'coding-agent:repo_git:abc_3');
  assert.equal(chunkId('a_b', 'x~y_z', 12), 'a~5Fb_x~7Ey~5Fz_12');
});

test('bank id validation follows the server rule', () => {
  assert.ok(validBankId('coding-agent:grimoire'));
  assert.ok(!validBankId('Upper'));
  assert.ok(!validBankId('-lead'));
  assert.ok(!validBankId('a'.repeat(65)));
});

test('tag and date filters apply to the loaded page', () => {
  const facts = [
    { id: 'a', tags: ['Support'], occurred_start: '2024-05-02T00:00:00Z' },
    { id: 'b', tags: [], mentioned_at: '2024-07-01T10:00:00Z' },
    { id: 'c', tags: ['support'] },
  ];
  assert.deepEqual(filterFacts(facts, { tag: 'support' }).map(f => f.id), ['a', 'c']);
  assert.deepEqual(filterFacts(facts, { from: '2024-06-01' }).map(f => f.id), ['b']);
  assert.deepEqual(filterFacts(facts, { to: '2024-05-31' }).map(f => f.id), ['a']);
});

test('mental model ids with folders are one path segment', async () => {
  const seen: string[] = [];
  const request = createClient({ fetch: async (url, init) => { seen.push(`${init?.method || 'GET'} ${url}`); return Response.json({}); } });
  const api = createBanksApi(request);
  await api.mentalModel('b', 'people/dana');
  await api.refreshMentalModel('b', 'people/dana');
  await api.updateMentalModel('b', 'people/dana', { folder: 'team' });
  await api.modelTree('b');
  assert.deepEqual(seen, ['GET /api/banks/b/mental-models/people%2Fdana', 'POST /api/banks/b/mental-models/people%2Fdana/refresh',
    'PATCH /api/banks/b/mental-models/people%2Fdana', 'GET /api/banks/b/mental-models-tree']);
});

test('rank table rows and arm order', () => {
  const trace = { arms: { keyword: [], semantic: [] }, ranks: { f1: { semantic: 1, graph: 4 }, f2: { keyword: 2 } } };
  const arms = armNames(trace);
  assert.deepEqual(arms, ['semantic', 'keyword', 'graph']);
  assert.deepEqual(rankRows(['f1', 'f2'], trace.ranks, arms), [{ id: 'f1', ranks: [1, null, 4] }, { id: 'f2', ranks: [null, 2, null] }]);
});

test('radial layout centres the chosen entity', () => {
  const pts = radialLayout([{ id: 'a', label: 'A', weight: 4 }, { id: 'b', label: 'B', weight: 1 }, { id: 'c', label: 'C', weight: 2 }], 'a', 200);
  assert.equal(pts[0]!.id, 'a');
  assert.equal(pts[0]!.x, 100);
  assert.ok(pts.every(p => p.x >= 0 && p.x <= 200 && p.y >= 0 && p.y <= 200));
});

test('operation kinds and stale reasons read as words', () => {
  assert.equal(opKindLabel('refresh_mental_model'), 'Refresh mental model');
  assert.equal(opKindLabel('something_new'), 'something_new');
  assert.equal(staleText('memories_changed'), 'memories changed since the last refresh');
});

test('model_required is told apart from other conflicts', async () => {
  const request = createClient({ fetch: async () => Response.json({ detail: 'model_required: no model', code: 'model_required' }, { status: 409 }) });
  const err = await createBanksApi(request).refreshMentalModel('b', 'm').catch(e => e);
  assert.ok(err instanceof ApiError);
  assert.equal(err.code, 'model_required');
  assert.ok(isModelRequired(err));
  assert.ok(!isModelRequired(new ApiError(409, '', 'a person wrote or edited this')));
});

test('built-in templates only use settings the server accepts', () => {
  const keys = new Set(CONFIG_KEYS.map(k => k.key).concat('retain_chunk_size'));
  assert.ok(CONFIG_TEXT_KEYS.every(k => !keys.has(k.key)));
  for (const t of builtinTemplates) {
    for (const [key, value] of Object.entries(t.manifest.bank?.config || {})) {
      assert.ok(keys.has(key), `${t.id}: ${key}`);
      assert.equal(typeof value, 'string');
    }
    const d = t.manifest.bank?.disposition;
    if (d) for (const v of Object.values(d)) assert.ok(v >= 1 && v <= 5);
  }
  assert.ok(builtinTemplates.some(t => t.id === 'coding-agent'));
});

test('a plain-text 404 means the route is missing; a missing bank does not', () => {
  assert.ok(isMissingRoute(new ApiError(404, '', 'Not Found')));
  assert.ok(isMissingRoute(new ApiError(405, '', 'Method Not Allowed')));
  assert.ok(isMissingRoute(new ApiError(404, '', 'Request failed (404)')));
  assert.ok(!isMissingRoute(new ApiError(404, '', 'no such bank')));
  assert.ok(!isMissingRoute(new ApiError(404, '', 'not found')));
  assert.ok(!isMissingRoute(new ApiError(500, '', 'boom')));
});

test('an older server: optional routes resolve to UNAVAILABLE and a template applies as profile fields', async () => {
  const seen: string[] = [];
  let sent: Record<string, unknown> = {};
  const request = createClient({ fetch: async (url, init) => {
    seen.push(`${init?.method || 'GET'} ${url}`);
    if (String(url) === '/api/banks' && init?.method === 'POST') { sent = JSON.parse(String(init.body)); return Response.json({ bank_id: 'x' }, { status: 201 }); }
    return new Response('404 page not found', { status: 404, statusText: 'Not Found' });
  } });
  const api = createBanksApi(request);
  assert.equal(await api.reflect('x', { query: 'q' }), UNAVAILABLE);
  assert.equal(await api.observations('x'), UNAVAILABLE);
  assert.equal(await api.operations('x'), UNAVAILABLE);
  assert.equal(await api.modelTree('x'), UNAVAILABLE);
  assert.equal(await api.directives('x'), UNAVAILABLE);
  assert.equal(await api.stats('x'), UNAVAILABLE);
  const templates = await api.templates();
  assert.ok(templates.every(t => t.builtin));
  const coding = templates.find(t => t.id === 'coding-agent');
  const out = await createFromTemplate(api, 'coding-agent:repo', coding, { name: 'Repo' });
  assert.equal(out.models, 0);
  assert.ok(!seen.some(s => s.includes('/import')));
  assert.equal(sent.name, 'Repo');
  assert.deepEqual(sent.directives, [{ text: 'When an answer rests on a past decision, say when it was made and why.', name: 'Cite decisions' }]);
  assert.equal((sent.disposition as { literalism: number }).literalism, 5);
});

test('create from a server template: the bank first, then an import of the manifest with the overrides', async () => {
  const calls: { url: string; body: Record<string, unknown> }[] = [];
  const template = { id: 'support', name: 'Support', description: '', manifest: { version: '1', bank: { mission: 'serve', config: { consolidation: 'auto' } },
    mental_models: [{ id: 'open-issues', name: 'Open issues', question: 'Which?' }], directives: [{ name: 'n', text: 't' }] } };
  const request = createClient({ fetch: async (url, init) => {
    calls.push({ url: `${init?.method} ${url}`, body: JSON.parse(String(init?.body)) });
    if (String(url).endsWith('/import')) return Response.json({ bank_id: 's', bank_created: false, config_applied: ['consolidation'], mental_models_created: ['open-issues'], mental_models_updated: [], directives_created: ['n'], directives_updated: [], operation_ids: [], dry_run: false });
    return Response.json({ bank_id: 's' }, { status: 201 });
  } });
  const out = await createFromTemplate(createBanksApi(request), 's', template, { mission: 'mine' });
  assert.equal(out.models, 1);
  assert.deepEqual(calls.map(c => c.url), ['POST /api/banks', 'POST /api/banks/s/import']);
  assert.deepEqual(calls[0]!.body, { bank_id: 's' });
  const manifest = (calls[1]!.body as { manifest: { bank: { mission: string }; mental_models: unknown[] } }).manifest;
  assert.equal(manifest.bank.mission, 'mine');
  assert.equal(manifest.mental_models.length, 1);
  assert.deepEqual(withOverrides({}, { name: 'N' }), { version: '1', bank: { name: 'N' } });
});

test('create without a template sends the profile fields', async () => {
  let sent: Record<string, unknown> = {};
  const request = createClient({ fetch: async (url, init) => {
    if (url === '/api/banks') { sent = JSON.parse(String(init?.body)); return Response.json({ bank_id: 'p' }, { status: 201 }); }
    return new Response('404 page not found', { status: 404 });
  } });
  await createFromTemplate(createBanksApi(request), 'p', undefined, { mission: 'm' });
  assert.deepEqual(sent, { bank_id: 'p', mission: 'm' });
});

test('retain, recall and reflect send the server\'s shapes', async () => {
  const bodies: Record<string, unknown>[] = [];
  const request = createClient({ fetch: async (url, init) => { bodies.push({ url, ...JSON.parse(String(init?.body)) }); return Response.json({ results: [], documents: [] }); } });
  const api = createBanksApi(request);
  await api.retain('b', [{ content: 'hello', document_id: 'd' }], { mode: 'chunks' });
  await api.retain('b', [{ content: 'later' }], { async: true });
  await api.recall('b', { query: 'q', budget: 'low', trace: true });
  await api.reflect('b', { query: 'q', fact_types: ['world'] });
  await api.reflect('b', { query: 'q', trace: true });
  assert.deepEqual(bodies[0], { url: '/api/banks/b/memories', items: [{ content: 'hello', document_id: 'd' }], mode: 'chunks' });
  assert.deepEqual(bodies[1], { url: '/api/banks/b/memories', items: [{ content: 'later' }], async: true });
  assert.deepEqual(bodies[2], { url: '/api/banks/b/memories/recall', query: 'q', budget: 'low', trace: true });
  assert.deepEqual(bodies[3], { url: '/api/banks/b/reflect', query: 'q', fact_types: ['world'], include: { facts: {} } });
  assert.deepEqual(bodies[4], { url: '/api/banks/b/reflect', query: 'q', include: { facts: {}, tool_calls: {} } });
});
