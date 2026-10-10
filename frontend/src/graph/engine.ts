/** WebGL graph engine: sigma renders nodes and edges; two 2D layers add cluster halos, focus and level-of-detail labels. */
import Graph from 'graphology';
import Sigma from 'sigma';
import type { NodeDisplayData, PartialButFor } from 'sigma/types';
import type { Settings } from 'sigma/settings';
import { buildCsr, folderOf, kindOf, neighbourhood, type Csr, type GraphData, type Kind } from './model';
import type { WorkerOut } from './graph.worker';
import { createNodeGlowProgram, type GlowStyle } from './glow';

export interface Theme { paper: string; ink: string; soft: string; accent: string; line: string; dark: boolean }
// `paper` here is the graph stage (what is behind the notes), which is darker than the app's page in dark themes.
export interface ClusterInfo { id: number; name: string; color: string; size: number }
export interface Filter { scope: 'linked' | 'local' | 'all'; current?: number; hops: number; folder: string; tag: string; kind: 'all' | Kind; cutoff: number | null }
export type ColorBy = 'cluster' | 'folder' | 'author';
export interface EngineEvents {
  hover(index: number | null): void;
  select(index: number | null): void;
  stats(nodes: number, edges: number): void;
  phase(phase: 'organising' | 'settling' | 'settled'): void;
  clusters(clusters: ClusterInfo[]): void;
  fail(message: string): void;
}

const DARK = ['#a78bfa', '#e879f9', '#2dd4bf', '#fbbf24', '#38bdf8', '#fb7185', '#a3e635', '#818cf8', '#fb923c', '#22d3ee', '#f0abfc'];
const LIGHT = ['#7c4dd8', '#c026d3', '#0d9488', '#d97706', '#0284c7', '#e11d48', '#65a30d', '#4f46e5', '#ea580c', '#0891b2', '#a21caf'];

export function readTheme(): Theme {
  const style = getComputedStyle(document.body);
  const get = (name: string, fallback: string) => style.getPropertyValue(name).trim() || fallback;
  const paper = get('--graph-stage', '') || get('--paper', '#fbf8f1');
  const m = /^#?([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})/i.exec(paper.length === 4 ? '#' + [...paper.slice(1)].map(c => c + c).join('') : paper);
  const luma = m ? (0.299 * parseInt(m[1]!, 16) + 0.587 * parseInt(m[2]!, 16) + 0.114 * parseInt(m[3]!, 16)) / 255 : 1;
  return { paper, ink: get('--ink', '#1f2530'), soft: get('--ink-soft', '#6b7280'), accent: get('--accent', '#7357c5'), line: get('--line', '#c9c1b1'), dark: luma < 0.5 };
}

function rgba(hex: string, alpha: number): string {
  let h = hex.trim();
  if (h.startsWith('#') && h.length === 4) h = '#' + [...h.slice(1)].map(c => c + c).join('');
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})/i.exec(h);
  if (!m) return hex;
  return `rgba(${parseInt(m[1]!, 16)},${parseInt(m[2]!, 16)},${parseInt(m[3]!, 16)},${alpha})`;
}

/** Later clusters reuse the palette with a rotated hue and alternating lightness so neighbours stay distinguishable. */
function shift(hex: string, round: number): string {
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})/i.exec(hex)!;
  const [r, g, b] = [1, 2, 3].map(k => parseInt(m[k]!, 16) / 255) as [number, number, number];
  const max = Math.max(r, g, b), min = Math.min(r, g, b), d = max - min;
  let h = d === 0 ? 0 : max === r ? ((g - b) / d) % 6 : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
  h = (h * 60 + 17 * round + 360) % 360;
  const l = Math.min(0.78, Math.max(0.32, (max + min) / 2 + (round % 2 ? -0.1 : 0.08))), sat = Math.min(0.85, d === 0 ? 0 : d / (1 - Math.abs(2 * ((max + min) / 2) - 1)));
  const c = (1 - Math.abs(2 * l - 1)) * sat, x = c * (1 - Math.abs(((h / 60) % 2) - 1)), o = l - c / 2;
  const [rr, gg, bb] = h < 60 ? [c, x, 0] : h < 120 ? [x, c, 0] : h < 180 ? [0, c, x] : h < 240 ? [0, x, c] : h < 300 ? [x, 0, c] : [c, 0, x];
  return '#' + [rr, gg, bb].map(v => Math.round((v + o) * 255).toString(16).padStart(2, '0')).join('');
}

/** Midpoint of two colours: a link between two groups belongs to both. */
function mix(a: string, b: string): string {
  const pa = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})/i.exec(a), pb = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})/i.exec(b);
  if (!pa || !pb) return a;
  return '#' + [1, 2, 3].map(k => Math.round((parseInt(pa[k]!, 16) + parseInt(pb[k]!, 16)) / 2).toString(16).padStart(2, '0')).join('');
}

const reducedMotion = () => typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;

export class GraphEngine {
  readonly data: GraphData;
  readonly csr: Csr;
  clusters: ClusterInfo[] = [];
  private sigma?: Sigma;
  private graph = new Graph({ type: 'undirected', multi: false });
  private worker?: Worker;
  private bg: HTMLCanvasElement; private fx: HTMLCanvasElement;
  private attrs: Record<string, unknown>[] = [];
  private rank: Float32Array = new Float32Array(0); private cluster: Int32Array = new Int32Array(0); private clusterCount = 0;
  private xy: Float32Array = new Float32Array(0);
  private visible: Uint8Array;
  private filter: Filter = { scope: 'all', hops: 1, folder: '', tag: '', kind: 'all', cutoff: null };
  private colorBy: ColorBy = 'cluster';
  private folders: string[] = [];
  private authorOf: Uint8Array = new Uint8Array(0);
  private glow: GlowStyle = { spread: 3, glow: 0.4, additive: 1, light: 0.3 };
  private veilFrom = 0; private bgDirty = true; private flowTimer = 0;
  private isolate = false; private depth = 1;
  private focusIndex = -1; private hoverIndex = -1;
  private active: { nodes: Int32Array; hops: Uint8Array } | null = null;
  private pulse = { index: -1, until: 0 };
  private collapsed = false; private userMoved = false; private destroyed = false;
  private halos: { cx: number; cy: number; r: number; count: number }[] = [];
  private drawQueued = false; private pending: Float32Array | null = null; private lastApply = 0; private applyCost = 0;
  private dpr = Math.min(devicePixelRatio || 1, 2);
  private resizeObserver: ResizeObserver;
  private folderRank: Int32Array;
  private paletteColors: string[] = [];

  constructor(private container: HTMLElement, data: GraphData, private theme: Theme, private events: EngineEvents) {
    performance.mark('graph:ctor'); this.data = data;
    (container as HTMLElement & { engine?: GraphEngine }).engine = this; // test hook
    this.csr = buildCsr(data.n, data.src, data.dst);
    this.visible = new Uint8Array(data.n).fill(1);
    const folders = [...new Set(data.ids.map(folderOf))].sort();
    const index = new Map(folders.map((f, i) => [f, i]));
    this.folderRank = Int32Array.from(data.ids, id => index.get(folderOf(id))!);
    this.folders = folders;
    this.authorOf = Uint8Array.from(data.ids, id => (kindOf(id) === 'memory' ? 1 : 0));
    container.style.position = 'relative';
    this.bg = this.layer('graph-halos', 0); this.fx = this.layer('graph-focus', 3);
    this.resizeObserver = new ResizeObserver(() => { if (!this.destroyed) { this.sizeLayers(); this.sigma?.resize(); this.sigma?.refresh(); this.schedule(); } });
    this.resizeObserver.observe(container);
    this.sizeLayers();
    this.events.phase('organising');
    performance.mark('graph:start');
    this.startWorker();
  }

  private layer(name: string, z: number) {
    const canvas = document.createElement('canvas');
    canvas.className = name;
    canvas.style.cssText = `position:absolute;inset:0;width:100%;height:100%;pointer-events:none;z-index:${z}`;
    this.container.appendChild(canvas);
    return canvas;
  }
  private sizeLayers() {
    const box = this.container.getBoundingClientRect();
    for (const canvas of [this.bg, this.fx]) { canvas.width = Math.max(1, Math.round(box.width * this.dpr)); canvas.height = Math.max(1, Math.round(box.height * this.dpr)); }
  }

  // ---- worker + graph construction ---------------------------------------------------------------------------
  private startWorker() {
    const { n, src, dst } = this.data;
    this.worker = new Worker(new URL('./graph.worker.ts', import.meta.url), { type: 'module' });
    this.worker.onmessage = (event: MessageEvent<WorkerOut>) => {
      if (this.destroyed) return;
      const msg = event.data;
      if (msg.type === 'ready') { performance.mark('graph:analysed'); void this.build(msg); } else { this.pending = msg.xy; if (msg.done) this.finalFrame = true; this.pump(); }
    };
    this.worker.postMessage({ type: 'init', n, src: src.slice(), dst: dst.slice(), folderKey: this.folderRank });
  }
  private finalFrame = false;

  private swatch(i: number): string {
    const base = (this.theme.dark ? DARK : LIGHT)[i % DARK.length]!;
    return i < DARK.length ? base : shift(base, Math.floor(i / DARK.length));
  }
  private palette(i: number): string {
    return i >= this.clusterCount ? (this.theme.dark ? '#7c7a8c' : '#9a96aa') : this.swatch(i);
  }
  /** The group a note is coloured by: its detected cluster, or its top-level folder. */
  private groupOf(i: number): number { return this.colorBy === 'folder' ? this.folderRank[i]! : this.colorBy === 'author' ? this.authorOf[i]! : this.cluster[i]!; }
  /** By author there are two groups: what you wrote (the brand purple) and what agents wrote (teal). */
  private groupColor(group: number): string { return this.colorBy === 'cluster' ? this.palette(group) : this.colorBy === 'author' ? this.swatch(group ? 2 : 0) : this.swatch(group); }
  private nodeColor(i: number): string { return this.groupColor(this.groupOf(i)); }
  /** Links inside a group take its colour; links between groups are a quieter blend of both, never a white line. */
  private edgeColor(a: number, b: number): string {
    const ga = this.groupOf(a), gb = this.groupOf(b), same = ga === gb;
    const color = same ? this.groupColor(ga) : mix(this.groupColor(ga), this.groupColor(gb));
    // sigma blends premultiplied colours but its edge shader does not premultiply, so a translucent edge adds light.
    // That is the look we want on a dark stage; on a light one it bleaches links to white, so premultiply by hand there.
    if (this.theme.dark) return rgba(color, this.edgeAlpha * (same ? 1 : 0.42));
    const alpha = Math.min(1, this.edgeAlpha * (same ? 1.5 : 0.8));
    const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})/i.exec(color);
    return m ? `rgba(${[1, 2, 3].map(k => Math.round(parseInt(m[k]!, 16) * alpha)).join(',')},${alpha})` : rgba(color, alpha);
  }
  /** Halo budget: generous on small graphs, tighter as the note count (and so the fill) grows. */
  private tuneGlow() {
    const n = this.data.n, dark = this.theme.dark;
    this.glow.spread = n > 12000 ? 1.8 : n > 3000 ? 2.4 : 3.2;
    this.glow.glow = (dark ? 0.42 : 0.2) * (n > 12000 ? 0.6 : 1);
    this.glow.additive = dark ? 1 : 0;
    this.glow.light = dark ? 0.32 : 0.16;
  }

  private async build(msg: Extract<WorkerOut, { type: 'ready' }>) {
    const { n, ids, titles } = this.data;
    this.byRank = Int32Array.from({ length: n }, (_, i) => i).sort((a, b) => msg.rank[b]! - msg.rank[a]!);
    this.rank = msg.rank; this.cluster = msg.cluster; this.clusterCount = msg.count; this.xy = msg.xy;
    const big = n > 4000;
    // a phone-sized stage has a quarter of the pixels: scale notes down with it or they merge into one blob
    const box = this.container.getBoundingClientRect();
    this.compact = Math.max(0.55, Math.min(1, Math.min(box.width, box.height) / 720));
    // Build in time-boxed slices so even a 50k-node graph never holds the main thread for a long task.
    const sliceBudget = 28;
    let sliceStart = performance.now();
    const yieldIfNeeded = async () => {
      if (performance.now() - sliceStart < sliceBudget) return;
      await new Promise(requestAnimationFrame);
      sliceStart = performance.now();
    };
    for (let i = 0; i < n; i++) {
      const r = Math.sqrt(this.rank[i]!);
      const size = ((big ? 1.6 : 2.6) + r * (big ? 7 : 11)) * this.compact;
      this.graph.addNode(String(i), { x: this.xy[i * 2]!, y: this.xy[i * 2 + 1]!, size, color: this.nodeColor(i), label: titles[i] || ids[i]! });
      this.attrs.push(this.graph.getNodeAttributes(String(i)));
      if ((i & 255) === 255) { await yieldIfNeeded(); if (this.destroyed) return; }
    }
    const { src, dst } = this.data;
    const drawn = this.backbone();
    const edgeAlpha = drawn.length < 400 ? 0.5 : drawn.length < 4000 ? 0.3 : 0.12;
    this.edgeAlpha = edgeAlpha * this.compact ** 1.5; // a small stage packs the same links into fewer pixels
    for (let k = 0; k < drawn.length; k++) {
      const e = drawn[k]!, a = src[e]!, b = dst[e]!;
      this.graph.addEdge(String(a), String(b), { size: 0.7, color: this.edgeColor(a, b) });
      if ((k & 1023) === 1023) { await yieldIfNeeded(); if (this.destroyed) return; }
    }
    performance.mark('graph:built');
    this.describeClusters();
    const settings: Partial<Settings> = {
      renderLabels: false, renderEdgeLabels: false, labelFont: 'system-ui, sans-serif', labelSize: 12, labelWeight: '500',
      labelColor: { color: this.theme.ink }, labelDensity: 0.7, labelGridCellSize: 120, labelRenderedSizeThreshold: 5.5,
      minCameraRatio: 0.015, maxCameraRatio: 8, hideEdgesOnMove: n > 12000, zIndex: false,
      defaultEdgeColor: rgba(this.theme.ink, 0.1), defaultNodeColor: this.theme.accent,
      defaultDrawNodeHover: () => undefined,
      defaultNodeType: 'glow', nodeProgramClasses: { glow: createNodeGlowProgram(this.glow) },
      nodeReducer: (key, data) => (this.visible[+key] ? data : { ...data, hidden: true }),
    };
    if (this.destroyed) return;
    this.tuneGlow();
    this.recompute(); // mask first, so sigma's first render is already filtered
    try {
      this.sigma = new Sigma(this.graph, this.container, settings);
    } catch (error) {
      this.events.fail(error instanceof Error ? error.message : String(error));
      return;
    }
    // sigma appended its canvases after ours; keep the halo layer underneath them and focus layer above
    // Label and hover layers are unused (labels and hover are drawn by our own layers). Each is a full-size canvas
    // the compositor must still blend every frame, which on a software rasteriser cost ~10x the frame rate; detached
    // canvases are never composited and sigma draws into them harmlessly. The mouse layer stays: it is the element
    // sigma listens on, so detaching it switches off drag, wheel and pinch.
    for (const name of ['edgeLabels', 'labels', 'hovers', 'hoverNodes']) this.container.querySelector(`canvas.sigma-${name}`)?.remove();
    this.container.insertBefore(this.bg, this.container.firstChild);
    this.container.appendChild(this.fx);
    this.wire();
    performance.mark('graph:sigma');
    this.events.clusters(this.groups());
    this.updateCollapse();
    this.events.phase('settling');
    this.sigma.once('afterRender', () => performance.mark('graph:frame'));
    performance.mark('graph:painted');
    this.container.dataset.rendered = String(n);
    this.updateZoomAttr();
    this.pump();
  }

  private edgeAlpha = 0.2;
  private compact = 1;
  /**
   * Edges handed to the GPU. Small and medium graphs draw every link. A very large one draws a backbone (each note's
   * strongest few links) because a million-pixel hairball carries no information; the full adjacency still drives
   * focus, hover and layout, so nothing is lost when you look closely.
   */
  private backbone(): Int32Array {
    const m = this.data.src.length, n = this.data.n, cap = 60000;
    if (m <= cap) return Int32Array.from({ length: m }, (_, e) => e);
    const { src, dst } = this.data, per = Math.max(1, Math.floor(cap / n));
    const keep = new Uint8Array(m), best = new Map<number, number[]>();
    const score = (e: number) => this.rank[src[e]!]! + this.rank[dst[e]!]! + (this.cluster[src[e]!] === this.cluster[dst[e]!] ? 0.5 : 0);
    for (let e = 0; e < m; e++) for (const v of [src[e]!, dst[e]!]) {
      let list = best.get(v); if (!list) best.set(v, list = []);
      list.push(e);
    }
    for (const list of best.values()) { list.sort((x, y) => score(y) - score(x)); for (let k = 0; k < Math.min(per, list.length); k++) keep[list[k]!] = 1; }
    const out: number[] = [];
    for (let e = 0; e < m; e++) if (keep[e]) out.push(e);
    return Int32Array.from(out);
  }

  private describeClusters() {
    const { n, ids, titles } = this.data;
    const members: number[][] = Array.from({ length: this.clusterCount + 1 }, () => []);
    for (let i = 0; i < n; i++) members[this.cluster[i]!]!.push(i);
    this.clusters = members.map((list, id) => {
      let name = id === this.clusterCount ? 'Other' : '';
      if (list.length && !name) {
        const top = list.reduce((best, i) => (this.rank[i]! > this.rank[best]! ? i : best), list[0]!);
        name = (titles[top] || ids[top]!).slice(0, 30);
      }
      this.clusterTop[id] = list.length && id < this.clusterCount ? list.reduce((best, i) => (this.rank[i]! > this.rank[best]! ? i : best), list[0]!) : -1;
      return { id, name, color: this.palette(id), size: list.length };
    });
  }
  private clusterTop: number[] = [];
  /** What the legend lists: clusters, or folders when colouring by folder. */
  private groups(): ClusterInfo[] {
    if (this.colorBy === 'cluster') return this.clusters;
    if (this.colorBy === 'author') {
      let agents = 0;
      for (let i = 0; i < this.data.n; i++) agents += this.authorOf[i]!;
      return [{ id: 0, name: 'Your notes', color: this.groupColor(0), size: this.data.n - agents }, { id: 1, name: 'Agent memory', color: this.groupColor(1), size: agents }].filter(g => g.size > 0);
    }
    const sizes = new Int32Array(this.folders.length);
    for (let i = 0; i < this.data.n; i++) sizes[this.folderRank[i]!]!++;
    return this.folders.map((name, id) => ({ id, name: name || 'Top level', color: this.swatch(id), size: sizes[id]! })).sort((a, b) => b.size - a.size);
  }
  setColorBy(mode: ColorBy) {
    if (mode === this.colorBy) return;
    this.colorBy = mode;
    this.recolor();
  }
  private recolor() {
    if (!this.sigma) return;
    for (let i = 0; i < this.data.n; i++) this.attrs[i]!.color = this.nodeColor(i);
    this.graph.forEachEdge((_edge, attrs, s, t) => { attrs.color = this.edgeColor(+s, +t); });
    this.sigma.refresh();
    this.events.clusters(this.groups());
    this.schedule();
  }

  // ---- layout frames -----------------------------------------------------------------------------------------
  private pump() {
    if (!this.sigma || !this.pending) return;
    if (this.applying) return;
    this.applying = true;
    requestAnimationFrame(() => {
      this.applying = false;
      if (this.destroyed || !this.pending || !this.sigma) return;
      const now = performance.now();
      // Spend at most about a fifth of the main thread on layout frames.
      if (!this.finalFrame && reducedMotion()) { this.pending = null; return; } // no animated settling for people who ask for less motion
      if (!this.finalFrame && this.applyCost > 45) { this.pending = null; return; } // too heavy to animate: wait for the final frame
      if (!this.finalFrame && now - this.lastApply < Math.max(0, this.applyCost * 4)) { this.pump(); return; }
      const xy = this.pending; this.pending = null;
      const start = performance.now();
      for (let i = 0; i < this.data.n; i++) { const a = this.attrs[i]!; a.x = xy[i * 2]!; a.y = xy[i * 2 + 1]!; }
      this.xy = xy; this.grid = null; this.areaCache = 0;
      if (!this.userMoved) this.fitBounds();
      this.sigma.refresh();
      if (!this.userMoved) this.sigma.getCamera().setState({ x: 0.5, y: 0.5, ratio: 1, angle: 0 });
      this.measureHalos();
      this.schedule();
      this.applyCost = performance.now() - start; this.lastApply = performance.now();
      if (this.finalFrame) { this.events.phase('settled'); this.finalFrame = false; } else if (this.pending) this.pump();
    });
  }
  private applying = false;

  /** Frame the camera's home view on what is visible, not on every note: hidden orphans must not leave blank margins. */
  private fitBounds() {
    if (!this.sigma) return;
    let x0 = Infinity, x1 = -Infinity, y0 = Infinity, y1 = -Infinity;
    for (let i = 0; i < this.data.n; i++) { if (!this.visible[i]) continue; const x = this.xy[i * 2]!, y = this.xy[i * 2 + 1]!; if (x < x0) x0 = x; if (x > x1) x1 = x; if (y < y0) y0 = y; if (y > y1) y1 = y; }
    if (!isFinite(x0)) return;
    const pad = Math.max((x1 - x0) * 0.06, (y1 - y0) * 0.06, 8);
    // labels hang off the right of their note, so leave them room: more of it on a narrow stage, where it is a larger share
    const right = pad + (x1 - x0) * (this.compact < 1 ? 0.2 : 0.06);
    this.sigma.setCustomBBox({ x: [x0 - pad, x1 + right], y: [y0 - pad, y1 + pad] });
  }

  private measureHalos() {
    const count = this.clusterCount + 1;
    const sx = new Float64Array(count), sy = new Float64Array(count), sn = new Float64Array(count);
    const { n } = this.data;
    for (let i = 0; i < n; i++) {
      if (!this.visible[i] || this.csr.offsets[i + 1]! === this.csr.offsets[i]!) continue;
      const c = this.cluster[i]!; sx[c]! += this.xy[i * 2]!; sy[c]! += this.xy[i * 2 + 1]!; sn[c]!++;
    }
    const sd = new Float64Array(count);
    for (let i = 0; i < n; i++) {
      if (!this.visible[i] || this.csr.offsets[i + 1]! === this.csr.offsets[i]!) continue;
      const c = this.cluster[i]!;
      sd[c]! += (this.xy[i * 2]! - sx[c]! / sn[c]!) ** 2 + (this.xy[i * 2 + 1]! - sy[c]! / sn[c]!) ** 2;
    }
    this.halos = Array.from({ length: count }, (_, c) => sn[c] ? { cx: sx[c]! / sn[c]!, cy: sy[c]! / sn[c]!, r: Math.sqrt(sd[c]! / sn[c]!) * 1.9 + 8, count: sn[c]! } : { cx: 0, cy: 0, r: 0, count: 0 });
  }

  // ---- interaction -------------------------------------------------------------------------------------------
  private wire() {
    const sigma = this.sigma!;
    // Sigma picks nodes by reading pixels back from the GPU on every pointer move, which stalls the main thread on
    // slow GPUs. Hit-testing is done against a spatial grid instead, so pointer moves cost microseconds.
    (sigma as unknown as { getNodeAtPosition: () => null }).getNodeAtPosition = () => null;
    const el = this.container;
    let down: { x: number; y: number; at: number } | null = null;
    let moveQueued = false, lastMove = { x: 0, y: 0 };
    const local = (e: PointerEvent) => { const r = el.getBoundingClientRect(); return { x: e.clientX - r.left, y: e.clientY - r.top }; };
    el.addEventListener('pointerdown', e => { this.userMoved = true; down = { ...local(e), at: performance.now() }; });
    el.addEventListener('pointerup', e => {
      const p = local(e);
      if (down && Math.hypot(p.x - down.x, p.y - down.y) < 6 && performance.now() - down.at < 600) this.tap(p.x, p.y, e.pointerType !== 'mouse');
      down = null;
    });
    el.addEventListener('pointermove', e => {
      if (e.pointerType !== 'mouse' || e.buttons || this.collapsed) return;
      lastMove = local(e);
      if (moveQueued) return;
      moveQueued = true;
      requestAnimationFrame(() => {
        moveQueued = false;
        const hit = this.nodeAt(lastMove.x, lastMove.y, 4);
        if (hit === this.hoverIndex) return;
        this.hoverIndex = hit; el.style.cursor = hit >= 0 ? 'pointer' : ''; this.activate(); this.events.hover(hit >= 0 ? hit : null);
      });
    });
    el.addEventListener('pointerleave', () => { if (this.hoverIndex >= 0) { this.hoverIndex = -1; el.style.cursor = ''; this.activate(); this.events.hover(null); } });
    el.addEventListener('wheel', () => { this.userMoved = true; }, { passive: true });
    sigma.getCamera().on('updated', () => { this.updateCollapse(); this.updateZoomAttr(); this.schedule(); });
  }

  private tap(x: number, y: number, touch: boolean) {
    if (this.collapsed) { const bubble = this.bubbleAt(x, y); if (bubble >= 0) this.flyToCluster(bubble); return; }
    const hit = this.nodeAt(x, y, touch ? 16 : 5);
    if (hit >= 0) this.focus(hit, this.depth, true); else if (this.focusIndex >= 0) this.focus(null);
  }

  // ---- hit testing -------------------------------------------------------------------------------------------
  private grid: { cell: number; x0: number; y0: number; cols: number; rows: number; head: Int32Array; next: Int32Array } | null = null;
  private buildGrid() {
    const { n } = this.data;
    let x0 = Infinity, x1 = -Infinity, y0 = Infinity, y1 = -Infinity;
    for (let i = 0; i < n; i++) { const x = this.xy[i * 2]!, y = this.xy[i * 2 + 1]!; if (x < x0) x0 = x; if (x > x1) x1 = x; if (y < y0) y0 = y; if (y > y1) y1 = y; }
    const side = Math.max(1, Math.ceil(Math.sqrt(n / 3)));
    const cell = Math.max((x1 - x0) / side, (y1 - y0) / side, 1e-6);
    const cols = Math.floor((x1 - x0) / cell) + 1, rows = Math.floor((y1 - y0) / cell) + 1;
    const head = new Int32Array(cols * rows).fill(-1), next = new Int32Array(n);
    for (let i = 0; i < n; i++) { const c = Math.floor((this.xy[i * 2]! - x0) / cell) + cols * Math.floor((this.xy[i * 2 + 1]! - y0) / cell); next[i] = head[c]!; head[c] = i; }
    this.grid = { cell, x0, y0, cols, rows, head, next };
  }
  /** The visible node under a viewport point (within its drawn radius plus `slack` px), or -1. */
  private nodeAt(px: number, py: number, slack: number): number {
    const sigma = this.sigma;
    if (!sigma) return -1;
    if (!this.grid) this.buildGrid();
    const g = this.grid!;
    const a = sigma.graphToViewport({ x: 0, y: 0 }), b = sigma.graphToViewport({ x: 1, y: 0 });
    const scale = Math.hypot(b.x - a.x, b.y - a.y) || 1;
    const reach = (14 + slack) / scale;
    const at = sigma.viewportToGraph({ x: px, y: py });
    const c0 = Math.max(0, Math.floor((at.x - reach - g.x0) / g.cell)), c1 = Math.min(g.cols - 1, Math.floor((at.x + reach - g.x0) / g.cell));
    const r0 = Math.max(0, Math.floor((at.y - reach - g.y0) / g.cell)), r1 = Math.min(g.rows - 1, Math.floor((at.y + reach - g.y0) / g.cell));
    let best = -1, bestScore = Infinity;
    for (let r = r0; r <= r1; r++) for (let c = c0; c <= c1; c++) {
      for (let i = g.head[c + r * g.cols]!; i >= 0; i = g.next[i]!) {
        if (!this.visible[i]) continue;
        const d = Math.hypot((this.xy[i * 2]! - at.x) * scale, (this.xy[i * 2 + 1]! - at.y) * scale);
        const radius = sigma.scaleSize(this.attrs[i]!.size as number);
        const score = d - radius;
        if (score <= slack && score < bestScore) { best = i; bestScore = score; }
      }
    }
    return best;
  }

  private updateZoomAttr() { if (this.sigma) this.container.dataset.zoom = (1 / this.sigma.getCamera().ratio).toFixed(3); }

  /** Collapse clusters into labelled bubbles when nodes are too dense on screen to be useful, with hysteresis. */
  private updateCollapse() {
    const sigma = this.sigma;
    if (!sigma || !this.visibleCount) return;
    const a = sigma.graphToViewport({ x: 0, y: 0 }), b = sigma.graphToViewport({ x: 1, y: 0 });
    const scale = Math.hypot(b.x - a.x, b.y - a.y);
    const spacing = Math.sqrt(this.extentArea() * scale * scale / this.visibleCount);
    const next = this.collapsed ? spacing < 9 : spacing < 5.5;
    if (next !== this.collapsed) {
      this.collapsed = next;
      if (next) { this.hoverIndex = -1; this.container.style.cursor = ''; }
    }
  }
  private areaCache = 0;
  private extentArea() {
    if (this.areaCache) return this.areaCache;
    let x0 = Infinity, x1 = -Infinity, y0 = Infinity, y1 = -Infinity;
    for (let i = 0; i < this.data.n; i++) { if (!this.visible[i]) continue; const x = this.xy[i * 2]!, y = this.xy[i * 2 + 1]!; if (x < x0) x0 = x; if (x > x1) x1 = x; if (y < y0) y0 = y; if (y > y1) y1 = y; }
    return this.areaCache = Math.max(1, (x1 - x0) * (y1 - y0));
  }
  private bubbleAt(x: number, y: number) {
    let best = -1, bestR = Infinity;
    this.bubbles().forEach(b => { const d = Math.hypot(b.x - x, b.y - y); if (d < b.r && b.r < bestR) { best = b.id; bestR = b.r; } });
    return best;
  }

  private visibleCount = 0;
  recompute() {
    const { n, ids, tags, t } = this.data;
    const f = this.filter;
    let local: Uint8Array | null = null;
    if (f.scope === 'local' && f.current !== undefined && f.current >= 0) { local = new Uint8Array(n); const nb = neighbourhood(this.csr, f.current, Math.max(1, f.hops)); for (const i of nb.nodes) local[i] = 1; }
    let isolate: { nodes: Int32Array } | null = this.isolate && this.active && this.focusIndex >= 0 ? this.active : null;
    const iso = isolate ? new Uint8Array(n) : null;
    if (iso && isolate) for (const i of isolate.nodes) iso[i] = 1;
    let count = 0;
    for (let i = 0; i < n; i++) {
      let ok = true;
      if (f.scope === 'linked') ok = this.csr.offsets[i + 1]! > this.csr.offsets[i]!;
      else if (f.scope === 'local') ok = !!local && local[i] === 1;
      if (ok && f.folder) ok = (folderOf(ids[i]!) || '/') === f.folder;
      if (ok && f.tag) ok = tags[i]!.includes(f.tag);
      if (ok && f.kind !== 'all') ok = kindOf(ids[i]!) === f.kind;
      if (ok && f.cutoff !== null && t[i]) ok = t[i]! <= f.cutoff;
      if (ok && iso) ok = iso[i] === 1;
      this.visible[i] = ok ? 1 : 0;
      count += ok ? 1 : 0;
    }
    this.visibleCount = count;
    let edges = 0;
    for (let e = 0; e < this.data.src.length; e++) edges += this.visible[this.data.src[e]!]! & this.visible[this.data.dst[e]!]!;
    this.areaCache = 0;
    this.fitBounds();
    this.sigma?.refresh();
    if (!this.userMoved) this.sigma?.getCamera().setState({ x: 0.5, y: 0.5, ratio: 1, angle: 0 });
    this.measureHalos();
    this.updateCollapse();
    this.events.stats(count, edges);
    this.schedule();
  }

  setFilter(patch: Partial<Filter>) {
    this.filter = { ...this.filter, ...patch };
    this.userMoved = false;
    if (this.refreshQueued) return;
    this.refreshQueued = true;
    requestAnimationFrame(() => { this.refreshQueued = false; if (!this.destroyed && this.sigma) this.recompute(); });
  }
  private refreshQueued = false;

  setDepth(depth: number) { this.depth = depth; if (this.focusIndex >= 0) this.focus(this.focusIndex, depth, true); }
  setIsolate(on: boolean) { this.isolate = on; if (this.focusIndex >= 0) { this.recompute(); if (on) this.frame(this.active!.nodes); else this.fit(); } }

  private activate() {
    const was = !!this.active;
    this.active = this.focusIndex >= 0 ? neighbourhood(this.csr, this.focusIndex, this.depth)
      : this.hoverIndex >= 0 ? neighbourhood(this.csr, this.hoverIndex, 1) : null;
    if (this.active && !was) this.veilFrom = performance.now();
    this.schedule();
  }

  focus(index: number | null, depth = this.depth, fly = true) {
    if (index === null || index < 0) {
      const wasIsolated = this.isolate && this.focusIndex >= 0;
      this.focusIndex = -1; this.activate();
      if (wasIsolated) this.recompute();
      this.events.select(null);
      return;
    }
    this.focusIndex = index; this.depth = depth; this.activate();
    this.events.select(index);
    if (this.isolate) this.recompute();
    if (fly && this.active) this.frame(this.isolate || this.active.nodes.length > 1 ? this.active.nodes : [index]);
  }

  /** Smoothly bring a set of nodes to fill most of the viewport. */
  frame(indices: ArrayLike<number>, maxRatio = 1) {
    const sigma = this.sigma;
    if (!sigma || !indices.length) return;
    let x0 = Infinity, x1 = -Infinity, y0 = Infinity, y1 = -Infinity;
    for (let k = 0; k < indices.length; k++) { const i = indices[k]!; if (!this.visible[i]) continue; const x = this.xy[i * 2]!, y = this.xy[i * 2 + 1]!; if (x < x0) x0 = x; if (x > x1) x1 = x; if (y < y0) y0 = y; if (y > y1) y1 = y; }
    if (!isFinite(x0)) return;
    const p0 = sigma.graphToViewport({ x: x0, y: y0 }), p1 = sigma.graphToViewport({ x: x1, y: y1 });
    const { width, height } = this.container.getBoundingClientRect();
    const bw = Math.max(1, Math.abs(p1.x - p0.x)), bh = Math.max(1, Math.abs(p1.y - p0.y));
    const camera = sigma.getCamera();
    const grow = Math.min((width * 0.74) / bw, (height * 0.74) / bh);
    const ratio = Math.min(maxRatio, Math.max(0.015, camera.ratio / grow, indices.length === 1 ? 0.3 : 0));
    const center = sigma.viewportToFramedGraph({ x: (p0.x + p1.x) / 2, y: (p0.y + p1.y) / 2 });
    this.userMoved = true;
    this.animate({ x: center.x, y: center.y, ratio });
  }
  private animate(state: { x: number; y: number; ratio: number }) {
    const camera = this.sigma!.getCamera();
    if (reducedMotion()) camera.setState({ ...state, angle: 0 }); else void camera.animate({ ...state, angle: 0 }, { duration: 650 });
  }

  fit() { this.userMoved = false; if (this.sigma) this.animate({ x: 0.5, y: 0.5, ratio: 1 }); }
  zoom(factor: number) { this.userMoved = true; const camera = this.sigma?.getCamera(); if (!camera) return; if (factor > 1) void camera.animatedZoom({ factor, duration: 220 }); else void camera.animatedUnzoom({ factor: 1 / factor, duration: 220 }); }
  /** Fly to a note and pulse a ring around it, without selecting it. */
  peek(index: number) {
    if (!this.visible[index]) return;
    this.pulse = { index, until: performance.now() + 1600 };
    this.frame([index]);
    this.schedule();
  }
  /** Fly to a legend entry: a cluster, or a folder when colouring by folder. */
  flyToGroup(id: number) {
    const members: number[] = [];
    for (let i = 0; i < this.data.n; i++) if (this.groupOf(i) === id && this.visible[i]) members.push(i);
    this.frame(members);
  }
  flyToCluster(id: number) {
    const members: number[] = [];
    for (let i = 0; i < this.data.n; i++) if (this.cluster[i] === id && this.visible[i]) members.push(i);
    this.frame(members);
  }
  /** Viewport position of a note, for tests and tooling. */
  screenPosition(index: number) { return this.sigma ? this.sigma.graphToViewport({ x: this.xy[index * 2]!, y: this.xy[index * 2 + 1]! }) : null; }
  isVisible(index: number) { return this.visible[index] === 1; }
  neighbours(index: number): number[] { return Array.from(this.csr.targets.subarray(this.csr.offsets[index]!, this.csr.offsets[index + 1]!)); }
  get settledCollapsed() { return this.collapsed; }

  setTheme(theme: Theme) {
    this.theme = theme;
    if (!this.sigma) return;
    this.clusters = this.clusters.map(c => ({ ...c, color: this.palette(c.id) }));
    this.tuneGlow();
    this.sigma.setSetting('labelColor', { color: theme.ink });
    this.sigma.setSetting('defaultEdgeColor', rgba(theme.ink, 0.1));
    this.recolor();
  }

  // ---- 2D layers ---------------------------------------------------------------------------------------------
  /** Queue one repaint of the overlay; `bg` also repaints the halo layer, which only changes with the camera, layout or filter. */
  private schedule(bg = true) {
    if (bg) this.bgDirty = true;
    if (this.drawQueued || this.destroyed) return;
    this.drawQueued = true;
    requestAnimationFrame(() => { this.drawQueued = false; this.draw(); });
  }

  private bubbles() {
    const sigma = this.sigma;
    if (!sigma) return [];
    const a = sigma.graphToViewport({ x: 0, y: 0 }), b = sigma.graphToViewport({ x: 1, y: 0 });
    const scale = Math.hypot(b.x - a.x, b.y - a.y);
    const out: { id: number; x: number; y: number; r: number; count: number }[] = [];
    this.halos.forEach((h, id) => { if (h.count && h.r) { const p = sigma.graphToViewport({ x: h.cx, y: h.cy }); out.push({ id, x: p.x, y: p.y, r: Math.max(5, h.r * scale * 0.8), count: h.count }); } });
    return out;
  }

  private draw() {
    const sigma = this.sigma;
    if (!sigma || this.destroyed) return;
    const { bg, fx, dpr, theme } = this;
    const width = fx.width / dpr, height = fx.height / dpr;
    const f = fx.getContext('2d')!;
    f.setTransform(dpr, 0, 0, dpr, 0, 0); f.clearRect(0, 0, width, height);
    const bubbles = this.bubbles();
    const placed: [number, number, number, number][] = [];
    const free = (x: number, y: number, w: number, h: number) => {
      for (const p of placed) if (x < p[0] + p[2] && x + w > p[0] && y < p[1] + p[3] && y + h > p[1]) return false;
      placed.push([x, y, w, h]); return true;
    };
    // Clusters are a property of the links, so their wash and bubbles keep cluster colours only while notes are coloured the same way.
    const tint = (id: number) => (this.colorBy === 'cluster' ? this.palette(id) : theme.soft);
    if (this.bgDirty) {
      this.bgDirty = false;
      const b = bg.getContext('2d')!;
      b.setTransform(dpr, 0, 0, dpr, 0, 0); b.clearRect(0, 0, width, height);
      // halos: a soft colour wash behind each community, which is what makes the clusters read at a glance
      if (this.colorBy === 'cluster' || this.collapsed) for (const bubble of bubbles) {
        const r = bubble.r * 1.45;
        if (bubble.id >= this.clusterCount || bubble.x < -r || bubble.y < -r || bubble.x > width + r || bubble.y > height + r) continue;
        const color = tint(bubble.id), peak = this.collapsed ? 0.5 : theme.dark ? 0.17 : 0.13;
        const g = b.createRadialGradient(bubble.x, bubble.y, 0, bubble.x, bubble.y, r);
        g.addColorStop(0, rgba(color, peak)); g.addColorStop(0.55, rgba(color, peak * 0.42)); g.addColorStop(1, rgba(color, 0));
        b.fillStyle = g; b.beginPath(); b.arc(bubble.x, bubble.y, r, 0, Math.PI * 2); b.fill();
      }
    }
    const showSet = this.active && !this.collapsed;
    if (showSet) this.drawSet(f, width, height, free);
    else if (this.collapsed) {
      // too dense to show notes: each cluster is one labelled bubble
      f.textAlign = 'center'; f.textBaseline = 'middle';
      for (const bubble of bubbles.sort((x, y) => y.count - x.count)) {
        const info = this.clusters[bubble.id];
        if (!info || bubble.x < 0 || bubble.y < 0 || bubble.x > width || bubble.y > height) continue;
        const color = tint(bubble.id);
        f.fillStyle = rgba(color, 0.42); f.strokeStyle = rgba(color, 0.9); f.lineWidth = 1.5;
        f.beginPath(); f.arc(bubble.x, bubble.y, bubble.r, 0, Math.PI * 2); f.fill(); f.stroke();
        const size = Math.min(18, Math.max(11, bubble.r / 3.2));
        f.font = `600 ${size}px system-ui, sans-serif`;
        const w = f.measureText(info.name).width;
        if (bubble.r > 14 && free(bubble.x - w / 2, bubble.y - size, w, size * 2.2)) {
          f.lineWidth = 3; f.strokeStyle = rgba(theme.paper, 0.85); f.strokeText(info.name, bubble.x, bubble.y - 2); f.fillStyle = theme.ink; f.fillText(info.name, bubble.x, bubble.y - 2);
          f.font = `500 ${size - 2}px system-ui, sans-serif`; f.fillStyle = theme.soft; f.fillText(String(bubble.count), bubble.x, bubble.y + size * 0.9);
        }
      }
    } else this.drawLabels(f, width, height, free);
    if (this.pulse.index >= 0) this.drawPulse(f);
  }

  /**
   * Level-of-detail labels: most important notes first, none overlapping, more of them the closer you zoom. The note
   * that anchors each cluster is set larger and in the cluster's colour, so the map has a readable hierarchy without a
   * second layer of floating cluster names; the note being edited is always labelled and ringed.
   */
  private drawLabels(f: CanvasRenderingContext2D, width: number, height: number, free: (x: number, y: number, w: number, h: number) => boolean) {
    const sigma = this.sigma!, { theme } = this;
    const zoom = Math.log2(1 / sigma.getCamera().ratio + 1);
    // fewer labels on a small stage: the budget follows the area there is to put them in
    const cap = Math.round(Math.min(220, 14 + 34 * zoom) * Math.max(0.4, Math.min(1, (width * height) / 700000)));
    f.textAlign = 'left'; f.textBaseline = 'middle';
    const here = this.filter.current !== undefined && this.filter.current >= 0 && this.visible[this.filter.current] ? this.filter.current : -1;
    const anchors = this.colorBy === 'cluster' ? this.clusterTop : [];
    let drawn = 0, looked = 0;
    const label = (i: number, lead: boolean) => {
      const p = sigma.graphToViewport({ x: this.xy[i * 2]!, y: this.xy[i * 2 + 1]! });
      if (p.x < -20 || p.y < -10 || p.x > width + 20 || p.y > height + 10) return;
      const r = sigma.scaleSize(this.attrs[i]!.size as number);
      const text = (this.data.titles[i] || this.data.ids[i]!).slice(0, 34);
      const big = lead || i === here;
      f.font = big ? '650 13px system-ui, sans-serif' : '500 12px system-ui, sans-serif';
      const key = big ? -i - 1 : i;
      let w = this.labelWidth.get(key);
      if (w === undefined) { w = f.measureText(text).width; this.labelWidth.set(key, w); }
      const x = p.x + r + 5;
      if (i === here) {
        f.strokeStyle = theme.accent; f.lineWidth = 1.5; f.globalAlpha = 0.9;
        f.beginPath(); f.arc(p.x, p.y, Math.max(4, r) + 4, 0, Math.PI * 2); f.stroke(); f.globalAlpha = 1;
      }
      if (!free(x - 2, p.y - 9, w + 4, 18)) return;
      // lesser labels recede, so the eye lands on hubs first
      f.globalAlpha = big ? 1 : drawn < cap * 0.4 ? 0.92 : 0.66;
      f.lineWidth = 3; f.strokeStyle = rgba(theme.paper, 0.9); f.strokeText(text, x, p.y);
      f.fillStyle = i === here ? theme.accent : lead ? mix(this.nodeColor(i), theme.ink) : theme.ink; f.fillText(text, x, p.y);
      f.globalAlpha = 1; drawn++;
    };
    if (here >= 0) label(here, false);
    for (const i of anchors) if (i >= 0 && i !== here && this.visible[i]) label(i, true);
    for (const i of this.byRank) {
      if (drawn >= cap || ++looked > 900) break;
      if (!this.visible[i] || i === here || (this.colorBy === 'cluster' && this.clusterTop[this.cluster[i]!] === i)) continue;
      label(i, false);
    }
  }
  private byRank: Int32Array = new Int32Array(0);
  private labelWidth = new Map<number, number>();

  private drawPulse(f: CanvasRenderingContext2D) {
    const left = this.pulse.until - performance.now();
    if (left <= 0) { this.pulse.index = -1; return; }
    const p = this.sigma!.graphToViewport({ x: this.xy[this.pulse.index * 2]!, y: this.xy[this.pulse.index * 2 + 1]! });
    const phase = (1 - left / 1600);
    for (let k = 0; k < 2; k++) {
      const t = (phase * 2 + k * 0.5) % 1;
      f.strokeStyle = rgba(this.theme.accent, (1 - t) * 0.8); f.lineWidth = 2;
      f.beginPath(); f.arc(p.x, p.y, 10 + t * 38, 0, Math.PI * 2); f.stroke();
    }
    this.schedule(false);
  }

  /**
   * Veil everything else, then redraw the focus neighbourhood crisply: full-opacity edges only where they matter.
   * A focused note also sends a slow stream of sparks down its links, which shows direction of attention at a glance.
   * That is the only continuous animation in the view; it runs at ~30 fps on the 2D overlay alone (the WebGL scene is
   * not re-rendered), stops with the focus, and never starts for people who ask for reduced motion.
   */
  private drawSet(f: CanvasRenderingContext2D, width: number, height: number, free: (x: number, y: number, w: number, h: number) => boolean) {
    const sigma = this.sigma!, set = this.active!, { theme } = this;
    const now = performance.now(), still = reducedMotion();
    const fade = still ? 1 : Math.min(1, (now - this.veilFrom) / 160);
    f.fillStyle = rgba(theme.paper, (theme.dark ? 0.76 : 0.8) * fade); f.fillRect(0, 0, width, height);
    const pos = new Map<number, { x: number; y: number }>();
    for (const i of set.nodes) { if (this.visible[i]) pos.set(i, sigma.graphToViewport({ x: this.xy[i * 2]!, y: this.xy[i * 2 + 1]! })); }
    const heavy = set.nodes.length > 4000;
    f.globalAlpha = fade; f.lineCap = 'round';
    for (const i of set.nodes) {
      const p = pos.get(i); if (!p) continue;
      const hop = set.hops[i]!;
      for (let e = this.csr.offsets[i]!; e < this.csr.offsets[i + 1]!; e++) {
        const j = this.csr.targets[e]!;
        if (j < i || set.hops[j] === 255) continue;
        const q = pos.get(j); if (!q) continue;
        const deep = Math.max(hop, set.hops[j]!);
        if (heavy && deep > 1) continue;
        f.strokeStyle = rgba(theme.accent, deep <= 1 ? 0.8 : deep === 2 ? 0.45 : 0.28);
        f.lineWidth = deep <= 1 ? 1.6 : 1;
        f.beginPath(); f.moveTo(p.x, p.y); f.lineTo(q.x, q.y); f.stroke();
      }
    }
    // sparks along the focused note's own links
    const centre = this.focusIndex >= 0 ? pos.get(this.focusIndex) : undefined;
    const first = centre ? this.csr.offsets[this.focusIndex]! : 0, last = centre ? this.csr.offsets[this.focusIndex + 1]! : 0;
    const flowing = !!centre && !still && last - first <= 300;
    if (flowing && centre) {
      const spark = theme.dark ? mix(theme.accent, '#ffffff') : theme.accent;
      for (let e = first; e < last; e++) {
        const j = this.csr.targets[e]!, q = pos.get(j);
        if (!q) continue;
        const t = (now / 1900 + (j * 0.618034) % 1) % 1;
        f.fillStyle = rgba(spark, Math.sin(Math.PI * t) * 0.95);
        f.beginPath(); f.arc(centre.x + (q.x - centre.x) * t, centre.y + (q.y - centre.y) * t, 1.9, 0, Math.PI * 2); f.fill();
      }
    }
    // nodes, deepest first so the focus sits on top
    const order = [...set.nodes].filter(i => pos.has(i)).sort((a, b) => set.hops[b]! - set.hops[a]!);
    const glowing = order.length <= 400;
    for (const i of order) {
      const p = pos.get(i)!, hop = set.hops[i]!, color = this.nodeColor(i);
      const r = Math.max(3, sigma.scaleSize(this.attrs[i]!.size as number) * (hop === 0 ? 1.35 : 1));
      if (glowing && hop <= 1) {
        const reach = r * (hop === 0 ? 4 : 2.8), g = f.createRadialGradient(p.x, p.y, r * 0.8, p.x, p.y, reach);
        g.addColorStop(0, rgba(color, theme.dark ? 0.45 : 0.3)); g.addColorStop(1, rgba(color, 0));
        f.fillStyle = g; f.beginPath(); f.arc(p.x, p.y, reach, 0, Math.PI * 2); f.fill();
      }
      f.fillStyle = color; f.beginPath(); f.arc(p.x, p.y, r, 0, Math.PI * 2); f.fill();
      if (hop === 0) { f.strokeStyle = theme.dark ? '#ffffff' : theme.accent; f.lineWidth = 2; f.beginPath(); f.arc(p.x, p.y, r + 3.5, 0, Math.PI * 2); f.stroke(); }
    }
    // labels: focus first, then by importance, skipping any that would overlap
    f.textBaseline = 'middle'; f.textAlign = 'left';
    const byRank = [...order].sort((a, b) => (set.hops[a]! === 0 ? -1 : set.hops[b]! === 0 ? 1 : this.rank[b]! - this.rank[a]!));
    let drawn = 0;
    for (const i of byRank) {
      if (drawn > 60) break;
      const p = pos.get(i)!;
      if (p.x < 0 || p.y < 0 || p.x > width || p.y > height) continue;
      const lead = set.hops[i] === 0;
      f.font = lead ? '650 13px system-ui, sans-serif' : '500 12px system-ui, sans-serif';
      const label = (this.data.titles[i] || this.data.ids[i]!).slice(0, 34), w = f.measureText(label).width;
      const x = p.x + Math.max(3, sigma.scaleSize(this.attrs[i]!.size as number)) * (lead ? 1.35 : 1) + 7;
      if (!free(x - 2, p.y - 9, w + 4, 18)) continue;
      f.lineWidth = 3; f.strokeStyle = rgba(theme.paper, 0.92); f.strokeText(label, x, p.y);
      f.fillStyle = lead ? (theme.dark ? '#ffffff' : theme.accent) : theme.ink; f.fillText(label, x, p.y); drawn++;
    }
    f.globalAlpha = 1;
    if (fade < 1) this.schedule(false);
    else if (flowing && !this.flowTimer) this.flowTimer = window.setTimeout(() => { this.flowTimer = 0; this.schedule(false); }, 33);
  }

  destroy() {
    this.destroyed = true;
    clearTimeout(this.flowTimer);
    this.worker?.postMessage({ type: 'stop' }); this.worker?.terminate();
    this.resizeObserver.disconnect();
    this.sigma?.kill();
    this.bg.remove(); this.fx.remove();
  }
}
