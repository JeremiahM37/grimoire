import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ApiError, createClient } from './api';
import { createBanksApi, createFromTemplate, isMissingRoute, UNAVAILABLE } from './banksApi';
import { armNames, buildTree, builtinTemplates, chunkId, CONFIG_KEYS, filterFacts, progressText, radialLayout, rankRows, validBankId } from './banksModel';

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

test('mental models group into a folder tree by slash', () => {
  const tree = buildTree([{ id: 'people/alice' }, { id: 'people/bob' }, { id: 'overview' }]);
  assert.deepEqual(tree.map(n => n.name), ['people', 'overview']);
  assert.deepEqual(tree[0]!.children.map(n => n.path), ['people/alice', 'people/bob']);
  assert.equal(tree[1]!.item?.id, 'overview');
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

test('progress text', () => {
  assert.equal(progressText({ stage: 'extract', processed: 2, total: 5 }), 'extract · 2 of 5');
  assert.equal(progressText(undefined), '');
});

test('built-in templates only use settings the server accepts', () => {
  const keys = new Set(CONFIG_KEYS.map(k => k.key).concat('retain_chunk_size'));
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

test('planned endpoints resolve to UNAVAILABLE on 404 and templates fall back to built-ins', async () => {
  const seen: string[] = [];
  const request = createClient({ fetch: async (url, init) => {
    seen.push(`${init?.method || 'GET'} ${url}`);
    if (String(url).startsWith('/api/banks') && init?.method === 'POST' && String(url) === '/api/banks') return Response.json({ bank_id: 'x' }, { status: 201 });
    return new Response('404 page not found', { status: 404, statusText: 'Not Found' });
  } });
  const api = createBanksApi(request);
  assert.equal(await api.reflect('x', { query: 'q' }), UNAVAILABLE);
  assert.equal(await api.observations('x'), UNAVAILABLE);
  assert.equal(await api.operations('x'), UNAVAILABLE);
  assert.equal(await api.mentalModels('x'), UNAVAILABLE);
  const templates = await api.templates();
  assert.equal(templates, builtinTemplates);
  const coding = templates.find(t => t.id === 'coding-agent');
  const out = await createFromTemplate(api, 'coding-agent:repo', coding, { name: 'Repo' });
  assert.equal(out.models, 0);
  assert.ok(seen.includes('POST /api/banks/coding-agent%3Arepo/mental-models'));
});

test('create from template sends the profile fields', async () => {
  let sent: Record<string, unknown> = {};
  const request = createClient({ fetch: async (url, init) => {
    if (url === '/api/banks') { sent = JSON.parse(String(init?.body)); return Response.json({ bank_id: 'p' }, { status: 201 }); }
    return new Response('404 page not found', { status: 404 });
  } });
  await createFromTemplate(createBanksApi(request), 'p', builtinTemplates.find(t => t.id === 'plain-retrieval'), { mission: 'm' });
  assert.deepEqual(sent, { bank_id: 'p', mission: 'm', config: { retain_extraction_mode: 'chunks', enable_graph: 'false', enable_temporal: 'false', enable_reranking: 'false' } });
});

test('retain sends top-level mode and recall posts its params', async () => {
  const bodies: Record<string, unknown>[] = [];
  const request = createClient({ fetch: async (url, init) => { bodies.push({ url, ...JSON.parse(String(init?.body)) }); return Response.json({ results: [], documents: [] }); } });
  const api = createBanksApi(request);
  await api.retain('b', [{ content: 'hello', document_id: 'd' }], 'chunks');
  await api.recall('b', { query: 'q', budget: 'low', trace: true });
  assert.deepEqual(bodies[0], { url: '/api/banks/b/memories', items: [{ content: 'hello', document_id: 'd' }], mode: 'chunks' });
  assert.deepEqual(bodies[1], { url: '/api/banks/b/memories/recall', query: 'q', budget: 'low', trace: true });
});
