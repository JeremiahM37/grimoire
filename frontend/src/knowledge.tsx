import { useEffect, useMemo, useRef, useState } from 'react';
import { createGrimoireApi } from './api';
import type { DocumentRecord, ExtractionResult, KnowledgeEdge, KnowledgeGraph, KnowledgeNode, KnowledgeQueryResult, KnowledgeSource } from './types';

export interface Point3D { x: number; y: number; z: number }
export interface Camera3D { yaw: number; pitch: number; distance: number; zoom: number; panX: number; panY: number }

export function projectPoint(point: Point3D, camera: Camera3D, width: number, height: number): { x: number; y: number; depth: number; scale: number } {
  const cy = Math.cos(camera.yaw), sy = Math.sin(camera.yaw), cp = Math.cos(camera.pitch), sp = Math.sin(camera.pitch);
  const x1 = point.x * cy - point.z * sy; const z1 = point.x * sy + point.z * cy;
  const y1 = point.y * cp - z1 * sp; const z2 = point.y * sp + z1 * cp;
  const depth = camera.distance + z2; const scale = camera.zoom * Math.min(width, height) * 0.42 / Math.max(80, depth);
  return { x: width / 2 + camera.panX + x1 * scale, y: height / 2 + camera.panY + y1 * scale, depth: z2, scale };
}

export function graphNodePositions(nodes: KnowledgeNode[], edges: KnowledgeEdge[] = []): Map<string, Point3D> {
  const positions = new Map<string, Point3D>();
  const sortedIds = nodes.map(node => node.id).sort();
  const goldenAngle = Math.PI * (3 - Math.sqrt(5));
  sortedIds.forEach((nodeId, index) => {
    const radius = 95 + Math.sqrt(index) * 35;
    const angle = index * goldenAngle;
    positions.set(nodeId, { x: Math.cos(angle) * radius, y: ((index % 7) - 3) * 42, z: Math.sin(angle) * radius });
  });
  const visibleEdges = edges.filter(edge => positions.has(edge.source) && positions.has(edge.target));
  const iterations = Math.min(36, Math.max(12, Math.floor(1800 / Math.max(1, nodes.length))));
  for (let iteration = 0; iteration < iterations; iteration += 1) {
    const forces = new Map<string, Point3D>(sortedIds.map(nodeId => [nodeId, { x: 0, y: 0, z: 0 }]));
    for (let firstIndex = 0; firstIndex < sortedIds.length; firstIndex += 1) {
      const firstId = sortedIds[firstIndex]!; const first = positions.get(firstId)!;
      for (let secondIndex = firstIndex + 1; secondIndex < sortedIds.length; secondIndex += 1) {
        const secondId = sortedIds[secondIndex]!; const second = positions.get(secondId)!;
        const delta = { x: first.x - second.x, y: first.y - second.y, z: first.z - second.z }; const distanceSquared = Math.max(100, delta.x ** 2 + delta.y ** 2 + delta.z ** 2); const force = 1500 / distanceSquared;
        const firstForce = forces.get(firstId)!; const secondForce = forces.get(secondId)!;
        firstForce.x += delta.x * force; firstForce.y += delta.y * force; firstForce.z += delta.z * force; secondForce.x -= delta.x * force; secondForce.y -= delta.y * force; secondForce.z -= delta.z * force;
      }
    }
    visibleEdges.forEach(edge => { const source = positions.get(edge.source)!; const target = positions.get(edge.target)!; const delta = { x: target.x - source.x, y: target.y - source.y, z: target.z - source.z }; const firstForce = forces.get(edge.source)!; const secondForce = forces.get(edge.target)!; firstForce.x += delta.x * 0.0018; firstForce.y += delta.y * 0.0018; firstForce.z += delta.z * 0.0018; secondForce.x -= delta.x * 0.0018; secondForce.y -= delta.y * 0.0018; secondForce.z -= delta.z * 0.0018; });
    sortedIds.forEach(nodeId => { const position = positions.get(nodeId)!; const force = forces.get(nodeId)!; position.x = Math.max(-360, Math.min(360, position.x + force.x - position.x * 0.0008)); position.y = Math.max(-260, Math.min(260, position.y + force.y - position.y * 0.0008)); position.z = Math.max(-360, Math.min(360, position.z + force.z - position.z * 0.0008)); });
  }
  return positions;
}

export function filteredKnowledgeNodes(graph: KnowledgeGraph | undefined, options: { showDocuments: boolean; showChunks: boolean; minDegree: number; dropNoisy: boolean; relation: string }): KnowledgeNode[] {
  if (!graph) return [];
  const degree = new Map<string, number>(); graph.edges.forEach(edge => { degree.set(edge.source, (degree.get(edge.source) || 0) + 1); degree.set(edge.target, (degree.get(edge.target) || 0) + 1); });
  const noisy = (node: KnowledgeNode) => node.label.trim().length < 2 || /^(unknown|other|misc|none|n\/a)$/i.test(node.label.trim());
  return graph.nodes.filter(node => (options.showDocuments || node.kind !== 'document') && (options.showChunks || node.kind !== 'chunk') && (degree.get(node.id) || 0) >= options.minDegree && (!options.dropNoisy || !noisy(node)) && (!options.relation || graph.edges.some(edge => edge.relation === options.relation && (edge.source === node.id || edge.target === node.id))));
}

export function filteredKnowledgeEdges(graph: KnowledgeGraph | undefined, relation: string): KnowledgeEdge[] {
  return (graph?.edges || []).filter(edge => !relation || edge.relation === relation);
}

export function batchPaths(paths: string[], batchSize = 10): string[][] {
  const batches: string[][] = [];
  for (let offset = 0; offset < paths.length; offset += batchSize) batches.push(paths.slice(offset, offset + batchSize));
  return batches;
}

export function highlightedGraph(graph: KnowledgeGraph | undefined, citations: Array<{ path: string; id?: string }> = [], selected?: { node?: string; edge?: string }): { nodes: Set<string>; edges: Set<string> } {
  const nodes = new Set<string>(); const edges = new Set<string>(); if (!graph) return { nodes, edges };
  citations.forEach(citation => { graph.nodes.filter(node => node.id === citation.id || node.path === citation.path).forEach(node => nodes.add(node.id)); graph.edges.filter(edge => edge.evidence.some(evidence => evidence.path === citation.path)).forEach(edge => { edges.add(edge.id); nodes.add(edge.source); nodes.add(edge.target); }); });
  if (selected?.node) nodes.add(selected.node); if (selected?.edge) { edges.add(selected.edge); const edge = graph.edges.find(item => item.id === selected.edge); if (edge) { nodes.add(edge.source); nodes.add(edge.target); } }
  return { nodes, edges };
}

type Selection = { node?: string; edge?: string };
const api = createGrimoireApi();

export function KnowledgeExplorer({ graph, result, close, open, onGraph, onResult }: { graph?: KnowledgeGraph; result?: KnowledgeQueryResult; close: () => void; open: (path: string) => void; onGraph: (graph: KnowledgeGraph) => void; onResult: (result: KnowledgeQueryResult | undefined) => void }) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const file = useRef<HTMLInputElement>(null);
  const [question, setQuestion] = useState('');
  const [relation, setRelation] = useState('');
  const [seed, setSeed] = useState('');
  const [after, setAfter] = useState('');
  const [before, setBefore] = useState('');
  const [depth, setDepth] = useState(2);
  const [minDegree, setMinDegree] = useState(0);
  const [showDocuments, setShowDocuments] = useState(true);
  const [showChunks, setShowChunks] = useState(false);
  const [dropNoisy, setDropNoisy] = useState(false);
  const [mode, setMode] = useState<'2d' | '3d'>('2d');
  const [selection, setSelection] = useState<Selection>();
  const [focusedCitation, setFocusedCitation] = useState<{ id?: string; path: string }>();
  const [source, setSource] = useState<KnowledgeSource>();
  const [docs, setDocs] = useState<DocumentRecord[]>([]);
  const [busy, setBusy] = useState(false);
  const [extracting, setExtracting] = useState(false);
  const [extraction, setExtraction] = useState<ExtractionResult[]>([]);
  const [message, setMessage] = useState('');
  const [, redraw] = useState(0);
  const camera = useRef<Camera3D>({ yaw: 0.25, pitch: -0.18, distance: 620, zoom: 1, panX: 0, panY: 0 });
  const drag = useRef<{ x: number; y: number; orbit: boolean; pan: boolean; node?: string } | undefined>(undefined);
  const nodes = useMemo(() => filteredKnowledgeNodes(graph, { showDocuments, showChunks, minDegree, dropNoisy, relation }), [graph, showDocuments, showChunks, minDegree, dropNoisy, relation]); const positions = useMemo(() => graphNodePositions(nodes, graph?.edges || []), [nodes, graph?.edges]); const relations = useMemo(() => [...new Set((graph?.edges || []).map(edge => edge.relation).filter(Boolean))], [graph]); const highlights = highlightedGraph(graph, focusedCitation ? [focusedCitation] : [], selection);
  const loadDocs = async () => { try { setDocs((await api.documents()).documents); } catch (e) { setMessage(e instanceof Error ? e.message : 'Documents unavailable'); } };
  useEffect(() => { void loadDocs(); }, []);
  const reloadGraph = async (nextRelation = relation) => { setBusy(true); try { onGraph(await api.knowledgeGraph({ seed: seed || undefined, depth, limit: 200, relation: nextRelation || undefined, min_degree: minDegree, document_visibility: showDocuments, drop_noisy: dropNoisy, chunk_visibility: showChunks })); } catch (e) { setMessage(e instanceof Error ? e.message : 'Knowledge graph unavailable'); } finally { setBusy(false); } };
  const ask = async () => { if (!question.trim()) return; setBusy(true); try { const next = await api.knowledgeQuery({ question: question.trim(), limit: 12, depth, after: after || undefined, before: before || undefined, expand: true }); onResult(next); onGraph(next.graph); } catch (e) { setMessage(e instanceof Error ? e.message : 'Knowledge query failed'); } finally { setBusy(false); } };
  const inspectSource = async (path: string) => { setSource(undefined); try { setSource(await api.knowledgeSource(path)); } catch (e) { setMessage(e instanceof Error ? e.message : 'Source unavailable'); } };
  const selectedNode = nodes.find(node => node.id === selection?.node);
  const selectedEdge = graph?.edges.find(edge => edge.id === selection?.edge);
  const selectedEvidence = selectedEdge?.evidence || (selectedNode ? (graph?.edges || []).filter(edge => edge.source === selectedNode.id || edge.target === selectedNode.id).flatMap(edge => edge.evidence) : []);
  const hitTest = (event: React.PointerEvent<HTMLCanvasElement>) => { const rect = event.currentTarget.getBoundingClientRect(); const x = (event.clientX - rect.left) * event.currentTarget.width / rect.width; const y = (event.clientY - rect.top) * event.currentTarget.height / rect.height; let hit: { node?: string; edge?: string; distance: number } = { distance: 24 }; for (const node of nodes) { const p = projectPoint(positions.get(node.id)!, mode === '2d' ? { ...camera.current, yaw: 0, pitch: 0 } : camera.current, event.currentTarget.width, event.currentTarget.height); const distance = Math.hypot(p.x - x, p.y - y); if (distance < hit.distance) hit = { node: node.id, distance }; } if (hit.node) return hit; for (const edge of graph?.edges || []) { const a = positions.get(edge.source), b = positions.get(edge.target); if (!a || !b || !nodes.some(node => node.id === edge.source) || !nodes.some(node => node.id === edge.target)) continue; const pa = projectPoint(a, mode === '2d' ? { ...camera.current, yaw: 0, pitch: 0 } : camera.current, event.currentTarget.width, event.currentTarget.height); const pb = projectPoint(b, mode === '2d' ? { ...camera.current, yaw: 0, pitch: 0 } : camera.current, event.currentTarget.width, event.currentTarget.height); const t = Math.max(0, Math.min(1, ((x - pa.x) * (pb.x - pa.x) + (y - pa.y) * (pb.y - pa.y)) / Math.max(1, (pb.x - pa.x) ** 2 + (pb.y - pa.y) ** 2))); const distance = Math.hypot(x - (pa.x + t * (pb.x - pa.x)), y - (pa.y + t * (pb.y - pa.y))); if (distance < hit.distance) hit = { edge: edge.id, distance }; } return hit;
  };
  useEffect(() => {
    const canvasElement = canvas.current;
    if (!canvasElement) return;
    const bounds = canvasElement.getBoundingClientRect();
    const width = Math.max(320, Math.floor(bounds.width * devicePixelRatio));
    const height = Math.max(320, Math.floor(bounds.height * devicePixelRatio));
    canvasElement.width = width; canvasElement.height = height;
    const context = canvasElement.getContext('2d');
    if (!context) return;
    const styles = getComputedStyle(canvasElement);
    const themeColor = (name: string, fallback: string) => styles.getPropertyValue(name).trim() || fallback;
    const colors = { paper: themeColor('--paper2', '#f7f0df'), ink: themeColor('--ink', '#111'), line: themeColor('--line', '#927'), accent: themeColor('--accent', '#b45309'), link: themeColor('--link', '#557da5'), good: themeColor('--good', '#4d8b62'), soft: themeColor('--ink-soft', '#745') };
    context.clearRect(0, 0, width, height); context.fillStyle = colors.paper; context.fillRect(0, 0, width, height);
    const view = mode === '2d' ? { ...camera.current, yaw: 0, pitch: 0 } : camera.current;
    const projected = new Map(nodes.map(node => [node.id, projectPoint(positions.get(node.id)!, view, width, height)]));
    const visibleEdges = filteredKnowledgeEdges(graph, relation).filter(edge => projected.has(edge.source) && projected.has(edge.target));
    visibleEdges.sort((first, second) => (projected.get(first.source)!.depth + projected.get(first.target)!.depth) - (projected.get(second.source)!.depth + projected.get(second.target)!.depth));
    context.font = `${12 * devicePixelRatio}px system-ui`;
    visibleEdges.forEach(edge => { const sourcePoint = projected.get(edge.source)!; const targetPoint = projected.get(edge.target)!; const highlighted = highlights.edges.has(edge.id); context.strokeStyle = highlighted ? colors.accent : colors.line; context.lineWidth = (highlighted ? 3 : 1) * devicePixelRatio; context.beginPath(); context.moveTo(sourcePoint.x, sourcePoint.y); context.lineTo(targetPoint.x, targetPoint.y); context.stroke(); context.fillStyle = highlighted ? colors.accent : colors.soft; context.fillText(edge.relation, (sourcePoint.x + targetPoint.x) / 2, (sourcePoint.y + targetPoint.y) / 2 - 5 * devicePixelRatio); });
    [...nodes].sort((first, second) => projected.get(first.id)!.depth - projected.get(second.id)!.depth).forEach(node => { const point = projected.get(node.id)!; const highlighted = highlights.nodes.has(node.id); const radius = (highlighted ? 18 : 13) * devicePixelRatio; context.fillStyle = highlighted ? colors.accent : node.kind === 'document' ? colors.link : node.kind === 'chunk' ? colors.good : colors.ink; context.beginPath(); context.arc(point.x, point.y, radius, 0, Math.PI * 2); context.fill(); context.fillStyle = colors.ink; context.fillText(node.label.slice(0, 22), point.x + radius + 4, point.y + 4); });
  }, [graph, nodes, positions, mode, highlights]);
  const move = (event: React.PointerEvent<HTMLCanvasElement>) => { let state = drag.current; if (!state && event.buttons) { state = { x: event.clientX, y: event.clientY, orbit: mode === '3d', pan: false }; drag.current = state; camera.current.yaw += 0.02; redraw(value => value + 1); return; } if (!state) return; const dx = event.clientX - state.x; const dy = event.clientY - state.y; state.x = event.clientX; state.y = event.clientY; if (state.node && !state.orbit && !state.pan) { const point = positions.get(state.node); if (point) { point.x += dx / Math.max(.2, camera.current.zoom); point.y += dy / Math.max(.2, camera.current.zoom); } } else if (state.orbit) { camera.current.yaw += dx * .008; camera.current.pitch = Math.max(-1.35, Math.min(1.35, camera.current.pitch + dy * .008)); } else { camera.current.panX += dx; camera.current.panY += dy; } redraw(value => value + 1); };
  const upload = async (fileToImport?: File, path?: string) => { if (!fileToImport) return; setBusy(true); try { await api.importDocument(fileToImport, path); await loadDocs(); setMessage(path ? 'Document replaced and re-indexed.' : 'Document imported and indexed.'); } catch (e) { setMessage(e instanceof Error ? e.message : 'Import failed'); } finally { setBusy(false); } };
  const extract = async (paths: string[]) => { if (!paths.length || extracting) return; setExtracting(true); setExtraction([]); setMessage(`Indexing relationships for ${paths.length} document${paths.length === 1 ? '' : 's'}…`); try { const results: ExtractionResult[] = []; for (const [batchIndex, batchPathsToIndex] of batchPaths(paths).entries()) { setMessage(`Indexing documents ${Math.min((batchIndex + 1) * 10, paths.length)} of ${paths.length}…`); const batch = await api.extractRelationships(batchPathsToIndex); results.push(...batch.results); setExtraction([...results]); } const failed = results.filter(result => result.status === 'error').length; setMessage(failed ? `${failed} document${failed === 1 ? '' : 's'} failed relationship indexing.` : `Relationship indexing complete for ${results.length} document${results.length === 1 ? '' : 's'}.`); } catch (e) { setMessage(e instanceof Error ? e.message : 'Relationship indexing failed'); } finally { setExtracting(false); } };
  return <div className="knowledge-explorer"><div className="knowledge-head"><div><p className="eyebrow">Evidence explorer</p><h2>Knowledge map <span id="knowledge-stat">{graph ? `${nodes.length} nodes · ${(graph.edges || []).length} relationships` : 'Loading…'}</span></h2><p className="panel-description">Explore bounded relationships with provenance-aware evidence.</p></div><button className="icon" aria-label="Close evidence explorer" onClick={close}>✕</button></div>
    <section className="knowledge-query"><label>Ask the corpus<input value={question} onChange={e => setQuestion(e.target.value)} onKeyDown={e => e.key === 'Enter' && void ask()} placeholder="What connects these ideas?" /></label><button className="btn primary" disabled={busy || !question.trim()} onClick={() => void ask()}>{busy ? 'Working…' : 'Ask with citations'}</button><label>After<input type="date" value={after} onChange={e => setAfter(e.target.value)} /></label><label>Before<input type="date" value={before} onChange={e => setBefore(e.target.value)} /></label></section>
    <section className="knowledge-controls"><label>Focus<input value={seed} onChange={e => setSeed(e.target.value)} onKeyDown={e => e.key === 'Enter' && void reloadGraph()} placeholder="entity or document id" /></label><label>Relationship<select value={relation} onChange={e => { const value = e.target.value; setRelation(value); void reloadGraph(value); }}><option value="">All relationships</option>{relations.map(item => <option key={item}>{item}</option>)}</select></label><label>Depth<select value={depth} onChange={e => setDepth(Number(e.target.value))}><option value="1">1 hop</option><option value="2">2 hops</option><option value="3">3 hops</option></select></label><label className="knowledge-check"><input type="checkbox" checked={showDocuments} onChange={e => setShowDocuments(e.target.checked)} /> Documents</label><label className="knowledge-check"><input type="checkbox" checked={showChunks} onChange={e => setShowChunks(e.target.checked)} /> Chunks</label><label>Min degree<input type="number" min="0" max="20" value={minDegree} onChange={e => setMinDegree(Math.max(0, Number(e.target.value) || 0))} /></label><label className="knowledge-check"><input type="checkbox" checked={dropNoisy} onChange={e => setDropNoisy(e.target.checked)} /> Drop noisy</label><button className="btn" onClick={() => void reloadGraph()} disabled={busy}>Refresh map</button><button className="btn" onClick={() => { camera.current = { yaw: 0.25, pitch: -0.18, distance: 620, zoom: 1, panX: 0, panY: 0 }; redraw(value => value + 1); }}>Reset view</button><button className="btn" aria-pressed={mode === '3d'} onClick={() => setMode(mode === '2d' ? '3d' : '2d')}>{mode === '2d' ? '3D view' : '2D view'}</button></section>
    {message && <p className="knowledge-message" role="status">{message}</p>}{graph?.truncated && <p className="knowledge-message">This view is bounded; focus a node or reduce depth to explore further.</p>}
    <p className="knowledge-help">{mode === '3d' ? 'Drag to orbit · Shift-drag to pan · Alt-drag a node to move it · wheel to zoom' : 'Drag to pan · drag a node to move it · wheel to zoom'}</p><div className="knowledge-workspace"><canvas ref={canvas} className="knowledge-canvas" tabIndex={0} aria-label={`Interactive ${mode} knowledge graph`} onKeyDown={event => { if (event.key === 'ArrowRight' || event.key === 'ArrowDown') { const index = Math.max(0, nodes.findIndex(node => node.id === selection?.node) + 1) % Math.max(1, nodes.length); setSelection(nodes[index] ? { node: nodes[index].id } : undefined); } if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') { const index = Math.max(0, nodes.findIndex(node => node.id === selection?.node) - 1); setSelection(nodes[index] ? { node: nodes[index].id } : undefined); } }} onPointerDown={event => { const hit = hitTest(event); const moveNode = Boolean(hit.node) && (mode === '2d' || event.altKey); drag.current = { x: event.clientX, y: event.clientY, orbit: mode === '3d' && !moveNode && !event.shiftKey, pan: mode === '3d' && event.shiftKey, node: moveNode ? hit.node : undefined }; event.currentTarget.setPointerCapture(event.pointerId); if (hit.node || hit.edge) setSelection(hit.node ? { node: hit.node } : { edge: hit.edge }); }} onPointerMove={move} onPointerUp={() => { drag.current = undefined; }} onPointerCancel={() => { drag.current = undefined; }} onWheel={event => { event.preventDefault(); camera.current.zoom = Math.max(.25, Math.min(4, camera.current.zoom * (event.deltaY < 0 ? 1.1 : .9))); redraw(value => value + 1); }} /></div><div className="knowledge-accessible-list" aria-label="Graph items">{nodes.map(node => <button key={node.id} className="citation" onClick={() => setSelection({ node: node.id })}><b>{node.label}</b><small>{node.kind}{node.path ? ` · ${node.path}` : ''}</small></button>)}{(graph?.edges || []).filter(edge => !relation || edge.relation === relation).map(edge => <button key={edge.id} className="citation" onClick={() => setSelection({ edge: edge.id })}><b>{edge.relation}</b><small>relationship evidence</small></button>)}</div>
    <aside className="knowledge-inspector" aria-live="polite">{selectedEdge ? <><h3>{selectedEdge.relation}</h3><p className="source-meta">Relationship evidence</p>{selectedEvidence.map(evidence => <button className="citation" key={evidence.path} onClick={() => void inspectSource(evidence.path)}><b>{evidence.title}</b><span>{evidence.text || 'Inspect source evidence'}</span><small>{evidence.path} · {evidence.origin} · {evidence.trust}</small></button>)}</> : selectedNode ? <><h3>{selectedNode.label}</h3><p className="badge">{selectedNode.kind}</p>{selectedNode.path && <button className="btn" onClick={() => void inspectSource(selectedNode.path!)}>Expand source</button>}{selectedNode.path && <button className="btn" onClick={() => void open(selectedNode.path!)}>Open source note</button>}{selectedEvidence.filter(evidence => evidence.path !== selectedNode.path).map(evidence => <button className="citation" key={evidence.path} onClick={() => void inspectSource(evidence.path)}><b>{evidence.title}</b><span>{evidence.text || 'Inspect relationship evidence'}</span><small>{evidence.path} · {evidence.origin} · {evidence.trust}</small></button>)}</> : <p>Select a node or relationship to inspect evidence.</p>}{source && <div className="source-expanded"><h4>{source.title}</h4><p className="source-meta">{source.path} · {source.origin} · {source.trust}</p>{source.truncated && <p className="knowledge-message">Source preview truncated at 1 MiB ({source.total ?? 'unknown'} bytes total).</p>}<blockquote>{source.text}</blockquote></div>}</aside>
    {result && <section className="knowledge-answer"><h3>Answer</h3><p>{result.answer}</p><div className="citation-list"><h4>Citations</h4>{result.citations.map(citation => <button className="citation" key={citation.id} onClick={() => { setFocusedCitation(citation); const matchingNode = graph?.nodes.find(node => node.id === citation.id || node.path === citation.path || node.label === citation.title); setSelection(matchingNode ? { node: matchingNode.id } : undefined); }}><b>{citation.title}</b><span>{citation.text}</span><small>{citation.path} · {citation.origin} · {citation.trust}</small></button>)}</div></section>}
    <section className="knowledge-documents"><div><h3>Documents</h3><p className="panel-description">Originals are preserved; extraction failures remain visible. Relationship indexing is an explicit model-assisted action.</p></div><input ref={file} type="file" accept=".md,.txt,.pdf,.docx" hidden onChange={e => void upload(e.target.files?.[0])} /><button className="btn" onClick={() => file.current?.click()} disabled={busy || extracting}>Import document</button><button className="btn" onClick={() => void extract([...new Set(nodes.filter(node => node.kind === 'document' && node.path).map(node => node.path!))])} disabled={busy || extracting || !nodes.some(node => node.kind === 'document')}>Index visible relationships</button>{selectedNode?.kind === 'document' && selectedNode.path && <button className="btn" onClick={() => void extract([selectedNode.path!])} disabled={busy || extracting}>Index selected</button>}{extraction.length > 0 && <p className="extraction-status" role="status">{extraction.map(item => `${item.path}: ${item.status}${item.status === 'error' ? ` (${item.error || 'unknown error'})` : ` · ${item.triples} triples`}`).join(' · ')}</p>}{docs.map(doc => <div className="document-row" key={doc.path}><span><b>{doc.title}</b><small>{doc.path} · {doc.format} · {doc.status}</small></span>{doc.error && <em>{doc.error}</em>}<a className="btn" href={api.originalDocument(doc.path)} target="_blank" rel="noreferrer">Original</a><button className="btn" onClick={() => { const input = document.createElement('input'); input.type = 'file'; input.accept = '.md,.txt,.pdf,.docx'; input.onchange = () => { const replacement = input.files?.[0]; if (replacement) void upload(replacement, doc.path); }; input.click(); }}>Replace</button><button className="btn" onClick={() => void api.refreshDocument(doc.path).then(loadDocs).catch(e => setMessage(String(e)))}>Refresh</button></div>)}</section>
  </div>;
}
