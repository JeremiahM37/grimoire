import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { Graph } from './types';

type GraphNode = Graph['nodes'][number] & { neighbors: Set<string>; x: number; y: number };
type Camera = { x: number; y: number; z: number };

function color(name: string, fallback: string) {
  return getComputedStyle(document.body).getPropertyValue(name).trim() || fallback;
}

/** Canvas drawing is imperative; all graph state and controls remain React-owned. */
export function GraphPanel({ graph, close, open, currentPath }: { graph?: Graph; close: () => void; open: (path: string) => void; currentPath?: string }) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const [scope, setScope] = useState<'linked' | 'local' | 'all'>('linked');
  const [query, setQuery] = useState('');
  const [searchIndex, setSearchIndex] = useState(-1);
  const [selected, setSelected] = useState<string>();
  const [camera, setCamera] = useState<Camera>({ x: 0, y: 0, z: 1 });
  const cameraRef = useRef(camera);
  cameraRef.current = camera;

  const all = useMemo(() => {
    const rows = new Map<string, GraphNode>();
    for (const node of graph?.nodes || []) rows.set(node.id, { ...node, neighbors: new Set(), x: 0, y: 0 });
    for (const edge of graph?.edges || []) {
      if (edge.src !== edge.dst && rows.has(edge.src) && rows.has(edge.dst)) {
        rows.get(edge.src)!.neighbors.add(edge.dst);
        rows.get(edge.dst)!.neighbors.add(edge.src);
      }
    }
    return rows;
  }, [graph]);
  const nodes = useMemo(() => {
    const active = currentPath ? all.get(currentPath) : undefined;
    return [...all.values()].filter(node => scope === 'all' || (scope === 'local' ? node.id === active?.id || active?.neighbors.has(node.id) : node.neighbors.size > 0)).map((node, index) => ({ ...node, x: Math.cos(index * 2.399963) * 58 * Math.sqrt(index + 1), y: Math.sin(index * 2.399963) * 58 * Math.sqrt(index + 1) }));
  }, [all, currentPath, scope]);
  const nodeMap = useMemo(() => new Map(nodes.map(node => [node.id, node])), [nodes]);
  const edges = useMemo(() => (graph?.edges || []).filter(edge => nodeMap.has(edge.src) && nodeMap.has(edge.dst)), [graph, nodeMap]);
  const hits = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? [...all.values()].filter(node => `${node.title} ${node.id}`.toLowerCase().includes(q)) : [];
  }, [all, query]);
  const selectedNode = selected ? all.get(selected) : undefined;

  const dimensions = useCallback(() => {
    const element = canvas.current;
    if (!element) return { width: 1, height: 1 };
    const rect = element.getBoundingClientRect();
    const dpr = Math.min(devicePixelRatio || 1, 2);
    element.width = Math.max(1, Math.round(rect.width * dpr));
    element.height = Math.max(1, Math.round(rect.height * dpr));
    return { width: rect.width, height: rect.height };
  }, []);
  const fit = useCallback(() => {
    const { width, height } = dimensions();
    if (!nodes.length) return setCamera({ x: width / 2, y: height / 2, z: 1 });
    const xs = nodes.map(node => node.x), ys = nodes.map(node => node.y);
    const z = Math.min(1.5, Math.max(.08, Math.min((width - 70) / (Math.max(...xs) - Math.min(...xs) + 80), (height - 70) / (Math.max(...ys) - Math.min(...ys) + 80))));
    setCamera({ x: width / 2 - (Math.max(...xs) + Math.min(...xs)) / 2 * z, y: height / 2 - (Math.max(...ys) + Math.min(...ys)) / 2 * z, z });
  }, [dimensions, nodes]);

  useEffect(() => { setSelected(undefined); setSearchIndex(-1); fit(); }, [fit, scope]);
  useEffect(() => {
    const element = canvas.current;
    if (!element) return;
    const observer = new ResizeObserver(() => fit());
    observer.observe(element);
    return () => observer.disconnect();
  }, [fit]);
  useEffect(() => {
    const element = canvas.current;
    if (!element) return;
    const { width, height } = dimensions();
    const context = element.getContext('2d');
    if (!context) return;
    const dpr = Math.min(devicePixelRatio || 1, 2);
    context.setTransform(dpr, 0, 0, dpr, 0, 0);
    context.clearRect(0, 0, width, height);
    const focus = selected;
    context.lineWidth = 1;
    for (const edge of edges) {
      const a = nodeMap.get(edge.src)!, b = nodeMap.get(edge.dst)!;
      context.globalAlpha = focus && focus !== a.id && focus !== b.id ? .18 : .65;
      context.strokeStyle = focus === a.id || focus === b.id ? color('--accent', '#7357c5') : color('--line', '#c9c1b1');
      context.beginPath(); context.moveTo(a.x * camera.z + camera.x, a.y * camera.z + camera.y); context.lineTo(b.x * camera.z + camera.x, b.y * camera.z + camera.y); context.stroke();
    }
    for (const node of nodes) {
      const x = node.x * camera.z + camera.x, y = node.y * camera.z + camera.y;
      const related = !focus || node.id === focus || all.get(focus)?.neighbors.has(node.id);
      context.globalAlpha = related ? 1 : .22;
      context.fillStyle = node.id === focus || node.id === currentPath ? color('--accent', '#7357c5') : color('--link', '#376b95');
      context.beginPath(); context.arc(x, y, 5 + Math.min(6, node.neighbors.size), 0, Math.PI * 2); context.fill();
      if (node.id === focus || (related && (nodes.length < 35 || camera.z > 1.3 || node.neighbors.size > 3))) {
        const title = (node.title || node.id).slice(0, 36);
        context.globalAlpha = 1; context.font = '12px system-ui'; context.textAlign = 'center'; context.fillStyle = color('--ink', '#1f2530'); context.fillText(title, x, y - 15);
      }
    }
    if (!nodes.length) { context.globalAlpha = 1; context.fillStyle = color('--ink', '#1f2530'); context.font = '15px system-ui'; context.textAlign = 'center'; context.fillText('No notes in this view. Try All notes.', width / 2, height / 2); }
    context.globalAlpha = 1;
    element.dataset.zoom = camera.z.toFixed(3);
  }, [all, camera, currentPath, dimensions, edges, nodeMap, nodes, selected]);
  useEffect(() => {
    const handle = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      event.preventDefault(); event.stopImmediatePropagation();
      if (query) { setQuery(''); setSearchIndex(-1); } else close();
    };
    addEventListener('keydown', handle, true);
    return () => removeEventListener('keydown', handle, true);
  }, [close, query]);

  const zoom = (factor: number, center?: { x: number; y: number }) => {
    const box = canvas.current?.getBoundingClientRect();
    const x = center?.x ?? (box?.width || 0) / 2, y = center?.y ?? (box?.height || 0) / 2;
    setCamera(old => { const z = Math.min(5, Math.max(.05, old.z * factor)); const ratio = z / old.z; return { x: x - (x - old.x) * ratio, y: y - (y - old.y) * ratio, z }; });
  };
  const focusNode = (id: string) => {
    const node = all.get(id); if (!node) return;
    if (!nodeMap.has(id)) setScope('all');
    requestAnimationFrame(() => {
      const target = nodeMap.get(id) || node;
      const rect = canvas.current?.getBoundingClientRect();
      setCamera(old => ({ x: (rect?.width || 0) / 2 - target.x * Math.max(old.z, 1), y: (rect?.height || 0) / 2 - target.y * Math.max(old.z, 1), z: Math.max(old.z, 1) }));
      setSelected(id);
    });
  };
  const openResult = (id: string) => { close(); open(id); };
  const pick = (event: React.PointerEvent<HTMLCanvasElement>) => {
    const rect = event.currentTarget.getBoundingClientRect(); const point = { x: event.clientX - rect.left, y: event.clientY - rect.top };
    let found: GraphNode | undefined; let best = 24;
    for (const node of nodes) { const distance = Math.hypot(node.x * camera.z + camera.x - point.x, node.y * camera.z + camera.y - point.y); if (distance < best) { best = distance; found = node; } }
    if (found) setSelected(found.id);
  };
  const pan = (event: React.PointerEvent<HTMLCanvasElement>) => {
    const element = event.currentTarget; element.setPointerCapture(event.pointerId);
    const start = { x: event.clientX, y: event.clientY, camera: cameraRef.current }; let moved = false;
    const move = (next: PointerEvent) => { const dx = next.clientX - start.x, dy = next.clientY - start.y; if (Math.hypot(dx, dy) > 4) moved = true; setCamera({ ...start.camera, x: start.camera.x + dx, y: start.camera.y + dy }); };
    const stop = (next: PointerEvent) => { element.removeEventListener('pointermove', move); element.removeEventListener('pointerup', stop); element.removeEventListener('pointercancel', stop); if (!moved) pick(next as unknown as React.PointerEvent<HTMLCanvasElement>); };
    element.addEventListener('pointermove', move); element.addEventListener('pointerup', stop); element.addEventListener('pointercancel', stop);
  };
  const onSearchKey = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Escape') { event.preventDefault(); setQuery(''); setSearchIndex(-1); return; }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') { event.preventDefault(); setSearchIndex(index => Math.max(0, Math.min(hits.length - 1, index + (event.key === 'ArrowDown' ? 1 : -1)))); return; }
    if (event.key === 'Enter' && hits.length) { event.preventDefault(); openResult(hits[Math.max(0, searchIndex)]!.id); }
  };

  return <div id="graph-modal" className="modal" role="dialog" aria-label="Note graph" onMouseDown={event => event.currentTarget === event.target && close()}><div className="modal-box graph-box"><button className="icon modal-close" onClick={close}>✕</button><h2>Graph <span id="graph-stat">{nodes.length} notes · {edges.length} links</span></h2><div className="graph-controls"><div className="note-search graph-search-wrap"><input id="graph-search" type="search" value={query} onChange={event => { setQuery(event.target.value); setSearchIndex(-1); }} onKeyDown={onSearchKey} placeholder="Search note titles…" aria-label="Search graph notes" autoFocus /><button id="graph-search-clear" className="icon" aria-label="Clear graph search" hidden={!query} onClick={() => { setQuery(''); setSearchIndex(-1); }}>✕</button></div><select id="graph-scope" aria-label="Graph scope" value={scope} onChange={event => setScope(event.target.value as typeof scope)}><option value="linked">Connected notes</option><option value="local">Current note &amp; neighbors</option><option value="all">All notes</option></select><button id="graph-out" className="icon" aria-label="Zoom out" onClick={() => zoom(1 / 1.3)}>−</button><button id="graph-in" className="icon" aria-label="Zoom in" onClick={() => zoom(1.3)}>+</button><button id="graph-fit" className="btn" onClick={fit}>Fit</button><button id="graph-reset" className="btn" onClick={() => setCamera(old => ({ ...old, z: 1 }))}>Reset</button></div><div className="graph-workspace"><canvas id="graph-canvas" ref={canvas} tabIndex={0} aria-label="Note graph. Drag to pan, scroll or use plus and minus to zoom. Search to select a note." onPointerDown={pan} onWheel={event => { event.preventDefault(); const rect = event.currentTarget.getBoundingClientRect(); zoom(Math.exp(-event.deltaY * .002), { x: event.clientX - rect.left, y: event.clientY - rect.top }); }} onKeyDown={event => { if (event.key === '+' || event.key === '=') { event.preventDefault(); zoom(1.3); } else if (event.key === '-') { event.preventDefault(); zoom(1 / 1.3); } else if (event.key === 'Home') { event.preventDefault(); fit(); } }} /><aside className="graph-inspector"><p id="graph-search-status" role="status">{query ? hits.length ? `${hits.length} results · Enter to open` : 'No matching notes. Try fewer words.' : ''}</p><div id="graph-results" aria-live="polite">{hits.map((node, index) => <div className={'graph-result' + (index === searchIndex ? ' kbd-sel' : '')} key={node.id}><button className="graph-result-open" onClick={() => openResult(node.id)}>{node.title || node.id}</button><button className="graph-show" aria-label={`Show connections for ${node.title || node.id}`} onClick={() => focusNode(node.id)}>Connections</button></div>)}</div><div id="graph-selection">{selectedNode ? <><strong>{selectedNode.title || selectedNode.id}</strong><button className="btn" onClick={() => openResult(selectedNode.id)}>Open note</button><span>{selectedNode.neighbors.size} connected notes</span>{[...selectedNode.neighbors].sort().map(id => <button className="graph-neighbor" key={id} onClick={() => focusNode(id)}>{all.get(id)?.title || id}</button>)}</> : 'Select a note to see its connections.'}</div></aside></div><p className="graph-help">Drag to pan · scroll to zoom · search and press Enter to open</p><button id="graph-close" onClick={close}>Close</button></div></div>;
}
