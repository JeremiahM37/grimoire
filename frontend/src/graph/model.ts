/** Pure graph data model: payload normalisation, adjacency and traversal. No DOM, so it runs in tests and workers. */

export interface RawGraph {
  v?: number;
  nodes?: { id: string; title: string }[];
  edges?: { src: string; dst: string }[] | number[];
  ids?: string[]; titles?: string[]; t?: number[]; tags?: string[][];
  unresolved?: string[];
}

export interface GraphData {
  n: number;
  ids: string[];
  titles: string[];
  /** Note date in unix seconds, 0 when unknown. */
  t: Float64Array;
  tags: string[][];
  /** Undirected, de-duplicated, loop-free edge endpoints. */
  src: Int32Array;
  dst: Int32Array;
  unresolved: string[];
}

export interface Csr { offsets: Int32Array; targets: Int32Array }

/** Accepts the legacy object payload and the columnar `?compact=1` payload. */
export function normalizeGraph(raw: RawGraph | undefined): GraphData {
  let ids: string[] = [], titles: string[] = [], tags: string[][] = [];
  let t: number[] | undefined;
  let a: number[] = [], b: number[] = [];
  if (raw?.ids) {
    ids = raw.ids; titles = raw.titles || ids; tags = raw.tags || []; t = raw.t;
    const flat = (raw.edges || []) as number[];
    for (let i = 0; i + 1 < flat.length; i += 2) { a.push(flat[i]!); b.push(flat[i + 1]!); }
  } else {
    const index = new Map<string, number>();
    for (const node of raw?.nodes || []) { index.set(node.id, ids.length); ids.push(node.id); titles.push(node.title); }
    for (const edge of (raw?.edges || []) as { src: string; dst: string }[]) {
      const x = index.get(edge.src), y = index.get(edge.dst);
      if (x !== undefined && y !== undefined) { a.push(x); b.push(y); }
    }
  }
  const n = ids.length;
  const seen = new Set<number>();
  const src: number[] = [], dst: number[] = [];
  for (let i = 0; i < a.length; i++) {
    let x = a[i]!, y = b[i]!;
    if (x === y || x < 0 || y < 0 || x >= n || y >= n) continue;
    if (x > y) [x, y] = [y, x];
    const key = x * n + y;
    if (seen.has(key)) continue;
    seen.add(key); src.push(x); dst.push(y);
  }
  const times = new Float64Array(n);
  if (t) for (let i = 0; i < n; i++) times[i] = t[i] || 0;
  return { n, ids, titles: titles.map((title, i) => title || ids[i]!), t: times, tags: ids.map((_, i) => tags[i] || []), src: Int32Array.from(src), dst: Int32Array.from(dst), unresolved: raw?.unresolved || [] };
}

export function buildCsr(n: number, src: Int32Array, dst: Int32Array, weight?: Float32Array): Csr & { weights?: Float32Array } {
  const offsets = new Int32Array(n + 1);
  for (let i = 0; i < src.length; i++) { offsets[src[i]! + 1]!++; offsets[dst[i]! + 1]!++; }
  for (let i = 0; i < n; i++) offsets[i + 1]! += offsets[i]!;
  const fill = offsets.slice(0, n);
  const targets = new Int32Array(src.length * 2);
  const weights = weight ? new Float32Array(src.length * 2) : undefined;
  for (let i = 0; i < src.length; i++) {
    const x = src[i]!, y = dst[i]!;
    const p = fill[x]!++, q = fill[y]!++;
    targets[p] = y; targets[q] = x;
    if (weights) { weights[p] = weight![i]!; weights[q] = weight![i]!; }
  }
  return { offsets, targets, weights };
}

/** Nodes within `depth` hops of `start`, as [index, hops] pairs ordered by hops. */
export function neighbourhood(csr: Csr, start: number, depth: number): { nodes: Int32Array; hops: Uint8Array } {
  const hops = new Uint8Array(csr.offsets.length - 1).fill(255);
  const queue: number[] = [start];
  hops[start] = 0;
  for (let head = 0; head < queue.length; head++) {
    const node = queue[head]!, h = hops[node]!;
    if (h >= depth) continue;
    for (let e = csr.offsets[node]!; e < csr.offsets[node + 1]!; e++) {
      const next = csr.targets[e]!;
      if (hops[next] === 255) { hops[next] = h + 1; queue.push(next); }
    }
  }
  return { nodes: Int32Array.from(queue), hops };
}

export function folderOf(id: string): string {
  const slash = id.indexOf('/');
  return slash > 0 ? id.slice(0, slash) : '';
}

export type Kind = 'note' | 'memory';
/** Agent memory lives in a few well-known folders; everything else is an ordinary note. */
export function kindOf(id: string): Kind {
  const folder = folderOf(id).toLowerCase();
  return folder === 'agent memory' || folder === 'memory' || folder === 'claude.ai memory' ? 'memory' : 'note';
}
