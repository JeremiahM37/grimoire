import assert from 'node:assert/strict';
import { test } from 'node:test';
import { batchPaths, filteredKnowledgeEdges, filteredKnowledgeNodes, graphNodePositions, highlightedGraph, projectPoint } from './knowledge';
import type { KnowledgeGraph } from './types';

const graph: KnowledgeGraph = { revision: 3, truncated: false, nodes: [
  { id: 'a', label: 'Alpha', kind: 'entity' }, { id: 'b', label: 'Brief', kind: 'document', path: 'brief.md' },
  { id: 'c', label: 'x', kind: 'entity' }, { id: 'chunk-1', label: 'chunk 1', kind: 'chunk', path: 'brief.md' },
], edges: [
  { id: 'ab', source: 'a', target: 'b', relation: 'supports', evidence: [{ path: 'brief.md', title: 'Brief', text: 'Alpha supports the brief.', origin: 'note', trust: 'trusted' }] },
  { id: 'ac', source: 'a', target: 'c', relation: 'mentions', evidence: [] },
] };

test('perspective projection changes depth and orbit changes screen position', () => {
  const camera = { yaw: 0, pitch: 0, distance: 500, zoom: 1, panX: 0, panY: 0 };
  const flat = projectPoint({ x: 80, y: 10, z: 0 }, camera, 800, 500);
  const deep = projectPoint({ x: 80, y: 10, z: 180 }, camera, 800, 500);
  assert.notEqual(flat.x, deep.x);
  assert.notEqual(flat.scale, deep.scale);
  const orbit = projectPoint({ x: 80, y: 10, z: 180 }, { ...camera, yaw: Math.PI / 2 }, 800, 500);
  assert.notEqual(orbit.x, deep.x);
});

test('graph filters hide documents/chunks, enforce degree, and drop noisy labels', () => {
  assert.deepEqual(filteredKnowledgeNodes(graph, { showDocuments: false, showChunks: false, minDegree: 2, dropNoisy: true, relation: '' }).map(n => n.id), ['a']);
  assert.deepEqual(filteredKnowledgeNodes(graph, { showDocuments: true, showChunks: true, minDegree: 0, dropNoisy: false, relation: 'supports' }).map(n => n.id), ['a', 'b']);
});

test('relationship filtering keeps every rendered edge on the selected relation', () => {
  assert.deepEqual(filteredKnowledgeEdges(graph, 'supports').map(edge => edge.relation), ['supports']);
  assert.deepEqual(filteredKnowledgeEdges(graph, '').map(edge => edge.relation), ['supports', 'mentions']);
});

test('relationship extraction batches never exceed ten paths', () => {
  const batches = batchPaths(Array.from({ length: 21 }, (_, index) => `note-${index}.md`));
  assert.deepEqual(batches.map(batch => batch.length), [10, 10, 1]);
  assert.equal(new Set(batches.flat()).size, 21);
});

test('citation and edge selection highlight a connected evidence set', () => {
  const highlighted = highlightedGraph(graph, [{ id: 'b', path: 'brief.md' }], { edge: 'ab' });
  assert.deepEqual([...highlighted.nodes].sort(), ['a', 'b', 'chunk-1']);
  assert.deepEqual([...highlighted.edges], ['ab']);
});

test('edge-driven positions are stable and pull connected nodes together', () => {
  const firstLayout = graphNodePositions(graph.nodes, graph.edges);
  const secondLayout = graphNodePositions(graph.nodes, graph.edges);
  assert.deepEqual([...firstLayout], [...secondLayout]);
  const connectedDistance = Math.hypot(firstLayout.get('a')!.x - firstLayout.get('b')!.x, firstLayout.get('a')!.y - firstLayout.get('b')!.y, firstLayout.get('a')!.z - firstLayout.get('b')!.z);
  const unrelatedDistance = Math.hypot(firstLayout.get('b')!.x - firstLayout.get('chunk-1')!.x, firstLayout.get('b')!.y - firstLayout.get('chunk-1')!.y, firstLayout.get('b')!.z - firstLayout.get('chunk-1')!.z);
  assert.ok(connectedDistance < unrelatedDistance);
});
