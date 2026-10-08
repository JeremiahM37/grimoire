/** Graph analytics shared by the layout worker and tests: PageRank, Louvain communities and a community-aware seed layout. */
import type { Csr } from './model';

/** PageRank by power iteration, scaled so the largest value is 1. */
export function pagerank(csr: Csr, iterations = 24, damping = 0.85): Float32Array {
  const n = csr.offsets.length - 1;
  if (!n) return new Float32Array(0);
  let rank = new Float32Array(n).fill(1 / n), next = new Float32Array(n);
  for (let it = 0; it < iterations; it++) {
    let dangling = 0;
    next.fill(0);
    for (let v = 0; v < n; v++) {
      const deg = csr.offsets[v + 1]! - csr.offsets[v]!;
      if (!deg) { dangling += rank[v]!; continue; }
      const share = rank[v]! / deg;
      for (let e = csr.offsets[v]!; e < csr.offsets[v + 1]!; e++) next[csr.targets[e]!]! += share;
    }
    const base = (1 - damping) / n + damping * dangling / n;
    for (let v = 0; v < n; v++) next[v] = base + damping * next[v]!;
    [rank, next] = [next, rank];
  }
  let max = 0;
  for (let v = 0; v < n; v++) max = Math.max(max, rank[v]!);
  for (let v = 0; v < n; v++) rank[v]! /= max || 1;
  return rank;
}

/** Louvain modularity optimisation on an undirected unit-weight graph; returns a community id per node (not renumbered by size). */
export function louvain(csr: Csr): Int32Array {
  const n0 = csr.offsets.length - 1;
  const membership = new Int32Array(n0).map((_, i) => i);
  let n = n0, offsets = csr.offsets, targets = csr.targets;
  let weights = new Float64Array(targets.length).fill(1);
  let self = new Float64Array(n);
  for (let level = 0; level < 12; level++) {
    const k = new Float64Array(n);
    let m2 = 0;
    for (let v = 0; v < n; v++) {
      let sum = self[v]!;
      for (let e = offsets[v]!; e < offsets[v + 1]!; e++) sum += weights[e]!;
      k[v] = sum; m2 += sum;
    }
    if (!m2) break;
    const comm = new Int32Array(n).map((_, i) => i);
    const total = Float64Array.from(k);
    const toward = new Float64Array(n);
    const touched: number[] = [];
    let improved = false;
    for (let pass = 0; pass < 24; pass++) {
      let moved = 0;
      for (let v = 0; v < n; v++) {
        const home = comm[v]!;
        touched.length = 0;
        for (let e = offsets[v]!; e < offsets[v + 1]!; e++) {
          const c = comm[targets[e]!]!;
          if (!toward[c]) touched.push(c);
          toward[c]! += weights[e]!;
        }
        total[home]! -= k[v]!;
        let best = home, bestGain = (toward[home] || 0) - total[home]! * k[v]! / m2;
        for (const c of touched) {
          const gain = toward[c]! - total[c]! * k[v]! / m2;
          if (gain > bestGain + 1e-12) { best = c; bestGain = gain; }
        }
        total[best]! += k[v]!;
        comm[v] = best;
        for (const c of touched) toward[c] = 0;
        toward[home] = 0;
        if (best !== home) moved++;
      }
      if (!moved) break;
      improved = true;
    }
    if (!improved) break;
    const renumber = new Int32Array(n).fill(-1);
    let count = 0;
    for (let v = 0; v < n; v++) if (renumber[comm[v]!]! < 0) renumber[comm[v]!] = count++;
    for (let v = 0; v < n; v++) comm[v] = renumber[comm[v]!]!;
    for (let v = 0; v < n0; v++) membership[v] = comm[membership[v]!]!;
    if (count === n) break;
    // aggregate: one super-node per community
    const order = new Int32Array(n), start = new Int32Array(count + 1);
    for (let v = 0; v < n; v++) start[comm[v]! + 1]!++;
    for (let c = 0; c < count; c++) start[c + 1]! += start[c]!;
    const fill = start.slice(0, count);
    for (let v = 0; v < n; v++) order[fill[comm[v]!]!++] = v;
    const nOffsets = new Int32Array(count + 1), nTargets: number[] = [], nWeights: number[] = [];
    const nSelf = new Float64Array(count), acc = new Float64Array(count), seen: number[] = [];
    for (let c = 0; c < count; c++) {
      seen.length = 0;
      for (let i = start[c]!; i < start[c + 1]!; i++) {
        const v = order[i]!;
        nSelf[c]! += self[v]!;
        for (let e = offsets[v]!; e < offsets[v + 1]!; e++) {
          const d = comm[targets[e]!]!;
          if (d === c) { nSelf[c]! += weights[e]!; continue; }
          if (!acc[d]) seen.push(d);
          acc[d]! += weights[e]!;
        }
      }
      for (const d of seen) { nTargets.push(d); nWeights.push(acc[d]!); acc[d] = 0; }
      nOffsets[c + 1] = nTargets.length;
    }
    n = count; offsets = nOffsets; targets = Int32Array.from(nTargets); weights = Float64Array.from(nWeights); self = nSelf;
  }
  return membership;
}

export interface Clusters {
  /** Cluster per node: 0..count-1 by descending size, `count` for isolated and small-fry nodes (the "other" bucket). */
  cluster: Int32Array;
  count: number;
  sizes: Int32Array;
}

/** Louvain communities, largest first, with tiny ones folded into one "other" bucket so the legend stays readable. */
export function clusterNodes(csr: Csr): Clusters {
  const n = csr.offsets.length - 1;
  const raw = louvain(csr);
  const size = new Map<number, number>();
  for (let v = 0; v < n; v++) if (csr.offsets[v + 1]! > csr.offsets[v]!) size.set(raw[v]!, (size.get(raw[v]!) || 0) + 1);
  const minSize = Math.max(3, Math.ceil(n * 0.004));
  const kept = [...size.entries()].filter(([, s]) => s >= minSize).sort((a, b) => b[1] - a[1] || a[0] - b[0]).slice(0, 120);
  const rank = new Map(kept.map(([c], i) => [c, i]));
  const count = kept.length;
  const cluster = new Int32Array(n);
  const sizes = new Int32Array(count + 1);
  for (let v = 0; v < n; v++) {
    const isolated = csr.offsets[v + 1]! === csr.offsets[v]!;
    cluster[v] = isolated ? count : rank.get(raw[v]!) ?? count;
    sizes[cluster[v]!]!++;
  }
  return { cluster, count, sizes };
}

export interface Seed { xy: Float32Array; centers: Float32Array; radii: Float32Array }

/** Deterministic starting layout: clusters as non-overlapping sunflower discs, important notes at the centre, orphans in an outer band. */
export function seedLayout(n: number, csr: Csr, clusters: Clusters, rank: Float32Array, folderKey: Int32Array): Seed {
  const { cluster, count, sizes } = clusters;
  const unit = 10, golden = 2.399963229728653;
  const radii = new Float32Array(count + 1), centers = new Float32Array((count + 1) * 2);
  const connectedOther = new Int32Array(1);
  for (let v = 0; v < n; v++) if (cluster[v] === count && csr.offsets[v + 1]! > csr.offsets[v]!) connectedOther[0]!++;
  const discSize = (c: number) => c === count ? connectedOther[0]! : sizes[c]!;
  const placed: number[] = [];
  const spiral = unit * 2;
  for (let c = 0; c <= count; c++) {
    const size = discSize(c);
    if (!size) continue;
    radii[c] = unit * (1.6 + Math.sqrt(size) * 1.25);
    let x = 0, y = 0;
    if (placed.length) {
      for (let step = 1; step < 20000; step++) {
        const r = spiral * Math.sqrt(step) * 1.6, a = step * golden;
        x = Math.cos(a) * r; y = Math.sin(a) * r;
        if (placed.every(p => Math.hypot(centers[p * 2]! - x, centers[p * 2 + 1]! - y) > radii[p]! + radii[c]! + unit)) break;
      }
    }
    centers[c * 2] = x; centers[c * 2 + 1] = y;
    placed.push(c);
  }
  // members of a cluster ordered by importance
  const members: number[][] = Array.from({ length: count + 1 }, () => []);
  const orphans: number[] = [];
  for (let v = 0; v < n; v++) {
    if (csr.offsets[v + 1]! === csr.offsets[v]!) orphans.push(v); else members[cluster[v]!]!.push(v);
  }
  const xy = new Float32Array(n * 2);
  for (let c = 0; c <= count; c++) {
    const list = members[c]!.sort((a, b) => rank[b]! - rank[a]!);
    const scale = list.length > 1 ? radii[c]! / Math.sqrt(list.length) : 0;
    list.forEach((v, i) => {
      const r = Math.sqrt(i + 0.5) * scale * 0.92, a = i * golden;
      xy[v * 2] = centers[c * 2]! + Math.cos(a) * r; xy[v * 2 + 1] = centers[c * 2 + 1]! + Math.sin(a) * r;
    });
  }
  let outer = 0;
  for (let c = 0; c <= count; c++) if (discSize(c)) outer = Math.max(outer, Math.hypot(centers[c * 2]!, centers[c * 2 + 1]!) + radii[c]!);
  orphans.sort((a, b) => folderKey[a]! - folderKey[b]! || a - b);
  placeOrphans(xy, orphans, outer);
  return { xy, centers, radii };
}

/** Unconnected notes sit in a tidy annulus just outside the connected layout, so they never tangle with it. */
export function placeOrphans(xy: Float32Array, orphans: ArrayLike<number>, radius: number) {
  const golden = 2.399963229728653, n = orphans.length;
  if (!n) return;
  const inner = radius * 1.12 + 12, outer = inner + Math.max(30, radius * 0.1 + Math.sqrt(n) * 7);
  for (let i = 0; i < n; i++) {
    const r = Math.sqrt(inner * inner + ((i + 0.5) / n) * (outer * outer - inner * inner)), a = i * golden, v = orphans[i]!;
    xy[v * 2] = Math.cos(a) * r; xy[v * 2 + 1] = Math.sin(a) * r;
  }
}
