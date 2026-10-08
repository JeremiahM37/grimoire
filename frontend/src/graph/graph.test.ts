import assert from 'node:assert/strict';
import test from 'node:test';
import { clusterNodes, louvain, pagerank, placeOrphans, seedLayout } from './analysis';
import { buildCsr, folderOf, kindOf, neighbourhood, normalizeGraph } from './model';

/** Three 8-cliques joined in a chain by single bridge links: an obvious planted partition. */
function planted() {
  const src: number[] = [], dst: number[] = [];
  for (let c = 0; c < 3; c++) for (let i = 0; i < 8; i++) for (let j = i + 1; j < 8; j++) { src.push(c * 8 + i); dst.push(c * 8 + j); }
  src.push(7, 15); dst.push(8, 16);
  return { n: 24, src: Int32Array.from(src), dst: Int32Array.from(dst) };
}

test('legacy and compact payloads normalise to the same graph', () => {
  const legacy = normalizeGraph({ nodes: [{ id: 'a.md', title: 'A' }, { id: 'd/b.md', title: 'B' }, { id: 'c.md', title: '' }], edges: [{ src: 'a.md', dst: 'd/b.md' }, { src: 'd/b.md', dst: 'a.md' }, { src: 'a.md', dst: 'a.md' }, { src: 'a.md', dst: 'ghost.md' }] });
  const compact = normalizeGraph({ v: 2, ids: ['a.md', 'd/b.md', 'c.md'], titles: ['A', 'B', ''], t: [5, 0, 7], tags: [['x'], [], []], edges: [0, 1, 1, 0, 0, 0, 0, 9] });
  for (const g of [legacy, compact]) {
    assert.equal(g.n, 3);
    assert.deepEqual([...g.src], [0]); assert.deepEqual([...g.dst], [1]); // duplicates, loops and dangling links dropped
    assert.equal(g.titles[2], 'c.md');
  }
  assert.deepEqual([...compact.t], [5, 0, 7]); assert.deepEqual(compact.tags[0], ['x']);
  assert.equal(normalizeGraph(undefined).n, 0);
});

test('folder and kind come from the path', () => {
  assert.equal(folderOf('Agent Memory/x.md'), 'Agent Memory'); assert.equal(folderOf('x.md'), '');
  assert.equal(kindOf('Agent Memory/x.md'), 'memory'); assert.equal(kindOf('journal/x.md'), 'note');
});

test('neighbourhood respects depth', () => {
  const csr = buildCsr(5, Int32Array.from([0, 1, 2, 3]), Int32Array.from([1, 2, 3, 4]));
  assert.deepEqual([...neighbourhood(csr, 0, 1).nodes].sort(), [0, 1]);
  assert.deepEqual([...neighbourhood(csr, 0, 3).nodes].sort(), [0, 1, 2, 3]);
  assert.equal(neighbourhood(csr, 0, 2).hops[2], 2);
  assert.equal(neighbourhood(csr, 0, 2).hops[4], 255);
});

test('louvain recovers a planted partition and clusters are ordered by size', () => {
  const { n, src, dst } = planted();
  const csr = buildCsr(n, src, dst);
  const comm = louvain(csr);
  for (let c = 0; c < 3; c++) assert.equal(new Set(Array.from({ length: 8 }, (_, i) => comm[c * 8 + i])).size, 1, `clique ${c} stays together`);
  assert.equal(new Set(comm).size, 3);
  const clusters = clusterNodes(csr);
  assert.equal(clusters.count, 3);
  assert.deepEqual([...clusters.sizes], [8, 8, 8, 0]);
});

test('isolated and tiny components fall into the other bucket', () => {
  const csr = buildCsr(6, Int32Array.from([0, 3]), Int32Array.from([1, 4]));
  const { cluster, count } = clusterNodes(csr);
  assert.equal(count, 0);
  assert.ok(cluster.every(c => c === 0));
});

test('pagerank puts the hub first and is scaled to 1', () => {
  const csr = buildCsr(6, Int32Array.from([0, 0, 0, 0, 0]), Int32Array.from([1, 2, 3, 4, 5]));
  const rank = pagerank(csr);
  assert.equal(Math.max(...rank), 1); assert.equal(rank[0], 1);
  assert.ok(rank[1]! < 1);
});

test('seed layout is finite, separates clusters and keeps orphans outside', () => {
  const { src, dst } = planted();
  const n = 30; // six orphans
  const csr = buildCsr(n, src, dst);
  const clusters = clusterNodes(csr), rank = pagerank(csr);
  const seed = seedLayout(n, csr, clusters, rank, new Int32Array(n));
  assert.ok(seed.xy.every(Number.isFinite));
  const centroid = (c: number) => { let x = 0, y = 0, k = 0; for (let v = 0; v < 24; v++) if (clusters.cluster[v] === c) { x += seed.xy[v * 2]!; y += seed.xy[v * 2 + 1]!; k++; } return [x / k, y / k]; };
  const [a, b] = [centroid(0), centroid(1)];
  assert.ok(Math.hypot(a![0]! - b![0]!, a![1]! - b![1]!) > 20, 'cluster centres are apart');
  const reach = Math.max(...Array.from({ length: 24 }, (_, v) => Math.hypot(seed.xy[v * 2]!, seed.xy[v * 2 + 1]!)));
  for (let v = 24; v < n; v++) assert.ok(Math.hypot(seed.xy[v * 2]!, seed.xy[v * 2 + 1]!) > reach, 'orphan is outside the connected layout');
});

test('placeOrphans leaves a clear margin around the given radius', () => {
  const xy = new Float32Array(20);
  placeOrphans(xy, Array.from({ length: 10 }, (_, i) => i), 100);
  for (let i = 0; i < 10; i++) assert.ok(Math.hypot(xy[i * 2]!, xy[i * 2 + 1]!) >= 112);
});
