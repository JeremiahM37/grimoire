import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { Graph } from './types';
import { GraphEngine, readTheme, type ClusterInfo, type Filter } from './graph/engine';
import { folderOf, normalizeGraph, type Kind } from './graph/model';

/** Importing this chunk (the Graph button is hovered) is a good moment to bring up the GPU process, which otherwise costs about 100 ms on first use. */
if (typeof requestIdleCallback === 'function') requestIdleCallback(() => { try { document.createElement('canvas').getContext('webgl2'); } catch { /* no WebGL: the panel reports it */ } });

/** The React shell owns controls and state; the WebGL engine owns drawing. */
export function GraphPanel({ graph, close, open, currentPath }: { graph?: Graph; close: () => void; open: (path: string) => void; currentPath?: string }) {
  const canvas = useRef<HTMLDivElement>(null);
  const engine = useRef<GraphEngine | undefined>(undefined);
  const data = useMemo(() => (graph ? normalizeGraph(graph) : undefined), [graph]);
  const [scope, setScope] = useState<Filter['scope']>('linked');
  const [folder, setFolder] = useState('');
  const [tag, setTag] = useState('');
  const [kind, setKind] = useState<'all' | Kind>('all');
  const [time, setTime] = useState(1000);
  const [playing, setPlaying] = useState(false);
  const [depth, setDepth] = useState(1);
  const [isolate, setIsolate] = useState(false);
  const [query, setQuery] = useState('');
  const [searchIndex, setSearchIndex] = useState(-1);
  const [selected, setSelected] = useState<number | null>(null);
  const [hover, setHover] = useState<number | null>(null);
  const [stat, setStat] = useState({ nodes: 0, edges: 0 });
  const [phase, setPhase] = useState<'organising' | 'settling' | 'settled'>('organising');
  const [clusters, setClusters] = useState<ClusterInfo[]>([]);
  const [failure, setFailure] = useState('');
  const [filtersOpen, setFiltersOpen] = useState(false); // only meaningful on narrow screens, where the filter row is collapsed

  const index = useMemo(() => new Map((data?.ids || []).map((id, i) => [id, i])), [data]);
  const current = currentPath ? index.get(currentPath) : undefined;
  const facets = useMemo(() => {
    const folders = new Map<string, number>(), tags = new Map<string, number>();
    let min = Infinity, max = -Infinity, dated = 0;
    for (let i = 0; i < (data?.n || 0); i++) {
      const f = folderOf(data!.ids[i]!) || '/';
      folders.set(f, (folders.get(f) || 0) + 1);
      for (const t of data!.tags[i]!) tags.set(t, (tags.get(t) || 0) + 1);
      const t = data!.t[i]!;
      if (t > 0) { dated++; min = Math.min(min, t); max = Math.max(max, t); }
    }
    const top = (m: Map<string, number>, k: number) => [...m.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, k);
    return { folders: top(folders, 40), tags: top(tags, 40), min, max, timeline: dated >= 20 && max - min > 2 * 86400, hasMemory: !!data?.ids.some(id => /^(agent memory|memory|claude\.ai memory)\//i.test(id)) };
  }, [data]);
  const cutoff = facets.timeline && time < 1000 ? facets.min + (facets.max - facets.min) * time / 1000 : null;

  // layout effect: start the worker before the browser paints the empty modal
  useLayoutEffect(() => {
    const element = canvas.current;
    if (!element || !data) return;
    setPhase('organising'); setFailure(''); setSelected(null); setHover(null); setClusters([]);
    const created = new GraphEngine(element, data, readTheme(), {
      hover: setHover, select: setSelected, clusters: setClusters, fail: setFailure,
      stats: (nodes, edges) => setStat({ nodes, edges }),
      phase: next => { setPhase(next); element.dataset.phase = next; },
    });
    engine.current = created;
    return () => { created.destroy(); engine.current = undefined; };
  }, [data]);
  useEffect(() => { engine.current?.setFilter({ scope, folder, tag, kind, cutoff, current }); }, [data, scope, folder, tag, kind, cutoff, current, phase === 'organising']);
  useEffect(() => { engine.current?.setDepth(depth); }, [depth]);
  useEffect(() => { engine.current?.setIsolate(isolate); }, [isolate]);
  // follow the app theme (light/dark toggles and the OS setting)
  useEffect(() => {
    let queued = false;
    const retheme = () => { if (queued) return; queued = true; requestAnimationFrame(() => { queued = false; engine.current?.setTheme(readTheme()); }); };
    const observer = new MutationObserver(retheme);
    observer.observe(document.documentElement, { attributes: true }); observer.observe(document.body, { attributes: true });
    const media = matchMedia('(prefers-color-scheme: dark)'); media.addEventListener('change', retheme);
    return () => { observer.disconnect(); media.removeEventListener('change', retheme); };
  }, []);
  // play the vault growing note by note
  useEffect(() => {
    if (!playing) return;
    const started = performance.now(), from = time >= 1000 ? 0 : time;
    let frame = 0;
    const step = (now: number) => {
      const value = Math.min(1000, from + (now - started) / 7000 * (1000 - from));
      setTime(Math.round(value));
      if (value < 1000) frame = requestAnimationFrame(step); else setPlaying(false);
    };
    frame = requestAnimationFrame(step);
    return () => cancelAnimationFrame(frame);
  }, [playing]);

  const hits = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q || !data) return [];
    const out: number[] = [];
    for (let i = 0; i < data.n && out.length < 50; i++) if (`${data.titles[i]} ${data.ids[i]}`.toLowerCase().includes(q)) out.push(i);
    return out;
  }, [data, query]);
  // search-to-fly: the camera glides to the best match as you type
  useEffect(() => {
    if (!hits.length) return;
    const timer = setTimeout(() => engine.current?.peek(hits[Math.max(0, searchIndex)]!), 280);
    return () => clearTimeout(timer);
  }, [hits, searchIndex]);
  useEffect(() => {
    const handle = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      event.preventDefault(); event.stopImmediatePropagation();
      if (query) { setQuery(''); setSearchIndex(-1); } else close();
    };
    addEventListener('keydown', handle, true);
    return () => removeEventListener('keydown', handle, true);
  }, [close, query]);

  const title = (i: number) => data!.titles[i] || data!.ids[i]!;
  const focusNode = useCallback((i: number) => {
    const run = () => engine.current?.focus(i, depth, true);
    if (engine.current?.isVisible(i)) run(); else { setScope('all'); setFolder(''); setTag(''); setKind('all'); setTime(1000); engine.current?.setFilter({ scope: 'all', folder: '', tag: '', kind: 'all', cutoff: null }); setTimeout(run, 60); }
  }, [depth]);
  const openResult = (i: number) => { close(); open(data!.ids[i]!); };
  const neighbours = selected !== null && engine.current ? engine.current.neighbours(selected).sort((a, b) => title(a).localeCompare(title(b))) : [];
  const onSearchKey = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Escape') { event.preventDefault(); setQuery(''); setSearchIndex(-1); return; }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') { event.preventDefault(); setSearchIndex(i => Math.max(0, Math.min(hits.length - 1, i + (event.key === 'ArrowDown' ? 1 : -1)))); return; }
    if (event.key === 'Enter' && hits.length) { event.preventDefault(); openResult(hits[Math.max(0, searchIndex)]!); }
  };
  const date = (cutoff ?? facets.max) ? new Date((cutoff ?? facets.max) * 1000).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' }) : '';
  const hovered = hover !== null && data ? title(hover) : '';

  return <div id="graph-modal" className="modal" role="dialog" aria-label="Note graph" onMouseDown={event => event.currentTarget === event.target && close()}><div className="modal-box graph-box"><button className="icon modal-close" onClick={close}>✕</button>
    <h2>Graph <span id="graph-stat">{stat.nodes} notes · {stat.edges} links</span><span id="graph-phase" role="status" aria-live="polite">{data && phase !== 'settled' ? (phase === 'organising' ? 'Organising…' : 'Settling…') : ''}</span></h2>
    <div className={'graph-controls' + (filtersOpen ? ' filters-open' : '')}>
      <div className="note-search graph-search-wrap"><input id="graph-search" type="search" value={query} onChange={event => { setQuery(event.target.value); setSearchIndex(-1); }} onKeyDown={onSearchKey} placeholder="Search note titles…" aria-label="Search graph notes" autoFocus={!matchMedia('(pointer: coarse)').matches} /><button id="graph-search-clear" className="icon" aria-label="Clear graph search" hidden={!query} onClick={() => { setQuery(''); setSearchIndex(-1); }}>✕</button></div>
      <select id="graph-scope" aria-label="Graph scope" value={scope} onChange={event => setScope(event.target.value as Filter['scope'])}><option value="linked">Connected notes</option><option value="local">Current note &amp; neighbors</option><option value="all">All notes</option></select>
      <button id="graph-filters-toggle" className="btn" aria-expanded={filtersOpen} onClick={() => setFiltersOpen(open => !open)}>Filters</button>
      <select className="graph-filter-part" id="graph-folder" aria-label="Filter by folder" value={folder} onChange={event => setFolder(event.target.value)}><option value="">All folders</option>{facets.folders.map(([name, count]) => <option key={name} value={name}>{name === '/' ? 'Top level' : name} ({count})</option>)}</select>
      {facets.tags.length > 0 && <select className="graph-filter-part" id="graph-tag" aria-label="Filter by tag" value={tag} onChange={event => setTag(event.target.value)}><option value="">All tags</option>{facets.tags.map(([name, count]) => <option key={name} value={name}>#{name} ({count})</option>)}</select>}
      {facets.hasMemory && <select className="graph-filter-part" id="graph-kind" aria-label="Filter by type" value={kind} onChange={event => setKind(event.target.value as typeof kind)}><option value="all">Notes &amp; memory</option><option value="note">Notes only</option><option value="memory">Agent memory only</option></select>}
      <button id="graph-out" className="icon" aria-label="Zoom out" onClick={() => engine.current?.zoom(1 / 1.5)}>−</button><button id="graph-in" className="icon" aria-label="Zoom in" onClick={() => engine.current?.zoom(1.5)}>+</button>
      <button id="graph-fit" className="btn" onClick={() => engine.current?.fit()}>Fit</button><button id="graph-reset" className="btn" onClick={() => { engine.current?.focus(null); setFolder(''); setTag(''); setKind('all'); setTime(1000); setIsolate(false); engine.current?.fit(); }}>Reset</button>
    </div>
    {facets.timeline && <div className={'graph-time graph-filter-part' + (filtersOpen ? ' open' : '')}><button id="graph-play" className="icon" aria-label={playing ? 'Pause timeline' : 'Play the vault growing'} onClick={() => { if (!playing && time >= 1000) setTime(0); setPlaying(p => !p); }}>{playing ? '❚❚' : '▶'}</button><input id="graph-time" type="range" min="0" max="1000" value={time} aria-label="Show notes created up to this date" onChange={event => { setPlaying(false); setTime(Number(event.target.value)); }} /><span id="graph-date">{date}</span></div>}
    <div className="graph-workspace"><div className="graph-stage"><div id="graph-canvas" ref={canvas} tabIndex={0} role="application" aria-label="Note graph. Drag to pan, scroll or pinch to zoom, click a note to focus its connections. Search to fly to a note." onKeyDown={event => { if (event.key === '+' || event.key === '=') { event.preventDefault(); engine.current?.zoom(1.5); } else if (event.key === '-') { event.preventDefault(); engine.current?.zoom(1 / 1.5); } else if (event.key === 'Home') { event.preventDefault(); engine.current?.fit(); } }} />
      {!data && <p className="graph-overlay" role="status">Loading graph…</p>}
      {failure && <p className="graph-overlay" role="alert">The graph needs WebGL, which this browser could not start ({failure}). Search still finds notes.</p>}
      {hovered && selected === null && <p className="graph-hover" aria-hidden="true">{hovered}</p>}
    </div>
    <aside className="graph-inspector"><p id="graph-search-status" role="status">{query ? hits.length ? `${hits.length} results · Enter to open` : 'No matching notes. Try fewer words.' : ''}</p>
      <div id="graph-results" aria-live="polite">{hits.map((node, position) => <div className={'graph-result' + (position === searchIndex ? ' kbd-sel' : '')} key={node}><button className="graph-result-open" onClick={() => openResult(node)}>{title(node)}</button><button className="graph-show" aria-label={`Show connections for ${title(node)}`} onClick={() => focusNode(node)}>Connections</button></div>)}</div>
      <div id="graph-selection">{selected !== null && data ? <><strong>{title(selected)}</strong><button className="btn" onClick={() => openResult(selected)}>Open note</button>
        <div className="graph-depth" role="group" aria-label="Connection depth">{[1, 2, 3].map(d => <button key={d} className={'btn' + (depth === d ? ' on' : '')} aria-pressed={depth === d} onClick={() => setDepth(d)}>{d} hop{d > 1 ? 's' : ''}</button>)}</div>
        <label className="graph-isolate"><input type="checkbox" checked={isolate} onChange={event => setIsolate(event.target.checked)} /> Show only this neighbourhood</label>
        <span>{neighbours.length} connected notes</span>{neighbours.map(id => <button className="graph-neighbor" key={id} onClick={() => focusNode(id)}>{title(id)}</button>)}</> : 'Click a note to see its connections.'}</div>
      {clusters.some(c => c.size > 1) && <div className="graph-legend" aria-label="Clusters"><h3>Clusters</h3>{clusters.filter(c => c.size > 1).slice(0, 40).map(c => <button key={c.id} className="graph-cluster" onClick={() => engine.current?.flyToCluster(c.id)}><i style={{ background: c.color }} />{c.name}<small>{c.size}</small></button>)}</div>}
    </aside></div>
    <p className="graph-help">Drag to pan · scroll or pinch to zoom · click a note to focus · search flies there</p><button id="graph-close" onClick={close}>Close</button></div></div>;
}
