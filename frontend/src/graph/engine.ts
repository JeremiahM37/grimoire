/** WebGL graph engine: sigma renders nodes and edges; two 2D layers add cluster halos, focus and level-of-detail labels. */
import Graph from 'graphology';
import Sigma from 'sigma';
import type { NodeDisplayData, PartialButFor } from 'sigma/types';
import type { Settings } from 'sigma/settings';
import { buildCsr, folderOf, kindOf, neighbourhood, type Csr, type GraphData, type Kind } from './model';
import type { WorkerOut } from './graph.worker';

export interface Theme { paper: string; ink: string; soft: string; accent: string; line: string; dark: boolean }
export interface ClusterInfo { id: number; name: string; color: string; size: number }
export interface Filter { scope: 'linked' | 'local' | 'all'; current?: number; folder: string; tag: string; kind: 'all' | Kind; cutoff: number | null }
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
  const paper = get('--paper', '#fbf8f1');
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
  private filter: Filter = { scope: 'all', folder: '', tag: '', kind: 'all', cutoff: null };
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

  private palette(i: number): string {
    if (i >= this.clusterCount) return this.theme.dark ? '#7c7a8c' : '#9a96aa';
    const base = (this.theme.dark ? DARK : LIGHT)[i % DARK.length]!;
    return i < DARK.length ? base : shift(base, Math.floor(i / DARK.length));
  }

  private async build(msg: Extract<WorkerOut, { type: 'ready' }>) {
    const { n, ids, titles } = this.data;
    this.byRank = Int32Array.from({ length: n }, (_, i) => i).sort((a, b) => msg.rank[b]! - msg.rank[a]!);
    this.rank = msg.rank; this.cluster = msg.cluster; this.clusterCount = msg.count; this.xy = msg.xy;
    const big = n > 4000;
    // Build in time-boxed slices so even a 50k-node graph never holds the main thread for a long task.
    const sliceBudget = 10;
    let sliceStart = performance.now();
    const yieldIfNeeded = async () => {
      if (performance.now() - sliceStart < sliceBudget) return;
      await new Promise(requestAnimationFrame);
      sliceStart = performance.now();
    };
    for (let i = 0; i < n; i++) {
      const r = Math.sqrt(this.rank[i]!);
      const size = (big ? 1.6 : 2.6) + r * (big ? 7 : 11);
      this.graph.addNode(String(i), { x: this.xy[i * 2]!, y: this.xy[i * 2 + 1]!, size, color: this.palette(this.cluster[i]!), label: titles[i] || ids[i]! });
      this.attrs.push(this.graph.getNodeAttributes(String(i)));
      if ((i & 255) === 255) { await yieldIfNeeded(); if (this.destroyed) return; }
    }
    const { src, dst } = this.data;
    const drawn = this.backbone();
    const edgeAlpha = drawn.length < 400 ? 0.5 : drawn.length < 4000 ? 0.3 : 0.12;
    this.edgeAlpha = edgeAlpha;
    for (let k = 0; k < drawn.length; k++) {
      const e = drawn[k]!, a = src[e]!, b = dst[e]!;
      const same = this.cluster[a] === this.cluster[b];
      this.graph.addEdge(String(a), String(b), { size: 0.6, color: same ? rgba(this.palette(this.cluster[a]!), edgeAlpha * 1.4) : rgba(this.theme.ink, edgeAlpha * 0.6) });
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
      nodeReducer: (key, data) => (this.visible[+key] ? data : { ...data, hidden: true }),
    };
    if (this.destroyed) return;
    this.recompute(); // mask first, so sigma's first render is already filtered
    try {
      this.sigma = new Sigma(this.graph, this.container, settings);
    } catch (error) {
      this.events.fail(error instanceof Error ? error.message : String(error));
      return;
    }
    // sigma appended its canvases after ours; keep the halo layer underneath them and focus layer above
    this.container.insertBefore(this.bg, this.container.firstChild);
    this.container.appendChild(this.fx);
    this.wire();
    performance.mark('graph:sigma');
    this.events.clusters(this.clusters);
    this.updateCollapse();
    this.events.phase('settling');
    this.sigma.once('afterRender', () => performance.mark('graph:frame'));
    performance.mark('graph:painted');
    this.container.dataset.rendered = String(n);
    this.updateZoomAttr();
    this.pump();
  }

  private edgeAlpha = 0.2;
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
      return { id, name, color: this.palette(id), size: list.length };
    });
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
    this.sigma.setCustomBBox({ x: [x0 - pad, x1 + pad], y: [y0 - pad, y1 + pad] });
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
    if (f.scope === 'local' && f.current !== undefined && f.current >= 0) { local = new Uint8Array(n); const nb = neighbourhood(this.csr, f.current, 1); for (const i of nb.nodes) local[i] = 1; }
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
    this.active = this.focusIndex >= 0 ? neighbourhood(this.csr, this.focusIndex, this.depth)
      : this.hoverIndex >= 0 ? neighbourhood(this.csr, this.hoverIndex, 1) : null;
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
    const edgeAlpha = this.edgeAlpha;
    for (let i = 0; i < this.data.n; i++) this.attrs[i]!.color = this.palette(this.cluster[i]!);
    this.graph.forEachEdge((edge, attrs, s, t) => {
      const a = +s, same = this.cluster[a] === this.cluster[+t];
      attrs.color = same ? rgba(this.palette(this.cluster[a]!), edgeAlpha * 1.4) : rgba(theme.ink, edgeAlpha * 0.6);
    });
    this.sigma.setSetting('labelColor', { color: theme.ink });
    this.sigma.setSetting('defaultEdgeColor', rgba(theme.ink, 0.1));
    this.sigma.refresh();
    this.events.clusters(this.clusters);
    this.schedule();
  }

  // ---- 2D layers ---------------------------------------------------------------------------------------------
  private schedule() {
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
    const b = bg.getContext('2d')!, f = fx.getContext('2d')!;
    b.setTransform(dpr, 0, 0, dpr, 0, 0); f.setTransform(dpr, 0, 0, dpr, 0, 0);
    b.clearRect(0, 0, width, height); f.clearRect(0, 0, width, height);
    const bubbles = this.bubbles();
    const placed: [number, number, number, number][] = [];
    const free = (x: number, y: number, w: number, h: number) => {
      for (const p of placed) if (x < p[0] + p[2] && x + w > p[0] && y < p[1] + p[3] && y + h > p[1]) return false;
      placed.push([x, y, w, h]); return true;
    };
    // halos: soft colour wash behind each community, which is what makes the clusters read at a glance
    for (const bubble of bubbles) {
      if (bubble.id >= this.clusterCount || bubble.x < -bubble.r || bubble.y < -bubble.r || bubble.x > width + bubble.r || bubble.y > height + bubble.r) continue;
      const color = this.palette(bubble.id);
      const r = bubble.r * 1.25;
      const g = b.createRadialGradient(bubble.x, bubble.y, 0, bubble.x, bubble.y, r);
      g.addColorStop(0, rgba(color, this.collapsed ? 0.5 : theme.dark ? 0.2 : 0.16)); g.addColorStop(1, rgba(color, 0));
      b.fillStyle = g; b.beginPath(); b.arc(bubble.x, bubble.y, r, 0, Math.PI * 2); b.fill();
    }
    const showSet = this.active && !this.collapsed;
    if (showSet) this.drawSet(f, width, height, free);
    // cluster names: map-style labels in the middle zoom range, bubbles when collapsed
    if (!showSet) {
      f.textAlign = 'center'; f.textBaseline = 'middle';
      for (const bubble of bubbles.sort((x, y) => y.count - x.count)) {
        const info = this.clusters[bubble.id];
        if (!info || bubble.x < 0 || bubble.y < 0 || bubble.x > width || bubble.y > height) continue;
        if (this.collapsed) {
          f.fillStyle = rgba(info.color, 0.42); f.strokeStyle = rgba(info.color, 0.9); f.lineWidth = 1.5;
          f.beginPath(); f.arc(bubble.x, bubble.y, bubble.r, 0, Math.PI * 2); f.fill(); f.stroke();
          const size = Math.min(18, Math.max(11, bubble.r / 3.2));
          f.font = `600 ${size}px system-ui, sans-serif`;
          const label = `${info.name}`;
          const w = f.measureText(label).width;
          if (bubble.r > 14 && free(bubble.x - w / 2, bubble.y - size, w, size * 2.2)) {
            f.lineWidth = 3; f.strokeStyle = rgba(theme.paper, 0.85); f.strokeText(label, bubble.x, bubble.y - 2); f.fillStyle = theme.ink; f.fillText(label, bubble.x, bubble.y - 2);
            f.font = `500 ${size - 2}px system-ui, sans-serif`; f.fillStyle = theme.soft; f.fillText(String(bubble.count), bubble.x, bubble.y + size * 0.9);
          }
        } else if (bubble.r > 70 && bubble.r < 700 && bubble.id < this.clusterCount) {
          const size = Math.min(26, Math.max(13, bubble.r / 6));
          f.font = `700 ${size}px system-ui, sans-serif`;
          const w = f.measureText(info.name).width;
          if (free(bubble.x - w / 2, bubble.y - bubble.r * 0.9 - size, w, size * 1.4)) { f.fillStyle = rgba(info.color, theme.dark ? 0.8 : 0.75); f.fillText(info.name, bubble.x, bubble.y - bubble.r * 0.9); }
        }
      }
    }
    if (!showSet && !this.collapsed) this.drawLabels(f, width, height, free);
    if (this.pulse.index >= 0) this.drawPulse(f);
  }

  /** Level-of-detail labels: most important notes first, none overlapping, more of them the closer you zoom. */
  private drawLabels(f: CanvasRenderingContext2D, width: number, height: number, free: (x: number, y: number, w: number, h: number) => boolean) {
    const sigma = this.sigma!, { theme } = this;
    const zoom = Math.log2(1 / sigma.getCamera().ratio + 1);
    const cap = Math.min(220, Math.round(14 + 34 * zoom));
    f.font = '500 12px system-ui, sans-serif'; f.textAlign = 'left'; f.textBaseline = 'middle';
    let drawn = 0, looked = 0;
    for (const i of this.byRank) {
      if (drawn >= cap || ++looked > 900) break;
      if (!this.visible[i]) continue;
      const p = sigma.graphToViewport({ x: this.xy[i * 2]!, y: this.xy[i * 2 + 1]! });
      if (p.x < -20 || p.y < -10 || p.x > width + 20 || p.y > height + 10) continue;
      const label = (this.data.titles[i] || this.data.ids[i]!).slice(0, 34);
      let w = this.labelWidth.get(i);
      if (w === undefined) { w = f.measureText(label).width; this.labelWidth.set(i, w); }
      const x = p.x + sigma.scaleSize(this.attrs[i]!.size as number) + 4;
      if (!free(x - 2, p.y - 8, w + 4, 16)) continue;
      f.lineWidth = 3; f.strokeStyle = rgba(theme.paper, 0.9); f.strokeText(label, x, p.y);
      f.fillStyle = theme.ink; f.fillText(label, x, p.y); drawn++;
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
    this.schedule();
  }

  /** Veil everything else, then redraw the focus neighbourhood crisply: full-opacity edges only where they matter. */
  private drawSet(f: CanvasRenderingContext2D, width: number, height: number, free: (x: number, y: number, w: number, h: number) => boolean) {
    const sigma = this.sigma!, set = this.active!, { theme } = this;
    f.fillStyle = rgba(theme.paper, theme.dark ? 0.78 : 0.8); f.fillRect(0, 0, width, height);
    const pos = new Map<number, { x: number; y: number }>();
    for (const i of set.nodes) { if (this.visible[i]) pos.set(i, sigma.graphToViewport({ x: this.xy[i * 2]!, y: this.xy[i * 2 + 1]! })); }
    const heavy = set.nodes.length > 4000;
    f.lineCap = 'round';
    for (const i of set.nodes) {
      const p = pos.get(i); if (!p) continue;
      const hop = set.hops[i]!;
      for (let e = this.csr.offsets[i]!; e < this.csr.offsets[i + 1]!; e++) {
        const j = this.csr.targets[e]!;
        if (j < i || set.hops[j] === 255) continue;
        const q = pos.get(j); if (!q) continue;
        const deep = Math.max(hop, set.hops[j]!);
        if (heavy && deep > 1) continue;
        f.strokeStyle = rgba(theme.accent, deep <= 1 ? 0.85 : deep === 2 ? 0.5 : 0.3);
        f.lineWidth = deep <= 1 ? 1.8 : 1.1;
        f.beginPath(); f.moveTo(p.x, p.y); f.lineTo(q.x, q.y); f.stroke();
      }
    }
    // nodes, deepest first so the focus sits on top
    const order = [...set.nodes].filter(i => pos.has(i)).sort((a, b) => set.hops[b]! - set.hops[a]!);
    for (const i of order) {
      const p = pos.get(i)!, hop = set.hops[i]!;
      const r = sigma.scaleSize(this.attrs[i]!.size as number) * (hop === 0 ? 1.35 : 1);
      f.fillStyle = this.palette(this.cluster[i]!); f.beginPath(); f.arc(p.x, p.y, Math.max(3, r), 0, Math.PI * 2); f.fill();
      if (hop === 0) { f.strokeStyle = theme.accent; f.lineWidth = 2.5; f.beginPath(); f.arc(p.x, p.y, Math.max(3, r) + 3, 0, Math.PI * 2); f.stroke(); }
    }
    // labels: focus first, then by importance, skipping any that would overlap
    f.textBaseline = 'middle'; f.textAlign = 'left'; f.font = '500 12px system-ui, sans-serif';
    const byRank = [...order].sort((a, b) => (set.hops[a]! === 0 ? -1 : set.hops[b]! === 0 ? 1 : this.rank[b]! - this.rank[a]!));
    let drawn = 0;
    for (const i of byRank) {
      if (drawn > 60) break;
      const p = pos.get(i)!;
      if (p.x < 0 || p.y < 0 || p.x > width || p.y > height) continue;
      const label = this.data.titles[i]!.slice(0, 34), w = f.measureText(label).width;
      const x = p.x + 9;
      if (!free(x - 2, p.y - 8, w + 4, 16)) continue;
      f.lineWidth = 3; f.strokeStyle = rgba(theme.paper, 0.92); f.strokeText(label, x, p.y);
      f.fillStyle = set.hops[i] === 0 ? theme.accent : theme.ink; f.fillText(label, x, p.y); drawn++;
    }
  }

  destroy() {
    this.destroyed = true;
    this.worker?.postMessage({ type: 'stop' }); this.worker?.terminate();
    this.resizeObserver.disconnect();
    this.sigma?.kill();
    this.bg.remove(); this.fx.remove();
  }
}
