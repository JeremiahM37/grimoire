/// <reference lib="webworker" />
/** Off-main-thread analysis and layout: PageRank, communities, a seeded layout, then ForceAtlas2 relaxation streamed back as position frames. */
import iterate from 'graphology-layout-forceatlas2/iterate';
import { clusterNodes, pagerank, placeOrphans, seedLayout } from './analysis';
import { buildCsr } from './model';

export interface WorkerInit { type: 'init'; n: number; src: Int32Array; dst: Int32Array; folderKey: Int32Array }
export type WorkerOut =
  | { type: 'ready'; rank: Float32Array; cluster: Int32Array; count: number; sizes: Int32Array; xy: Float32Array; centers: Float32Array; radii: Float32Array; ms: number }
  | { type: 'frame'; xy: Float32Array; iteration: number; done: boolean };

const scope = self as unknown as DedicatedWorkerGlobalScope;
let stopped = false;

scope.onmessage = (event: MessageEvent<WorkerInit | { type: 'stop' }>) => {
  if (event.data.type === 'stop') { stopped = true; return; }
  stopped = false;
  run(event.data);
};

function run({ n, src, dst, folderKey }: WorkerInit) {
  const started = performance.now();
  const csr = buildCsr(n, src, dst);
  const rank = pagerank(csr);
  const clusters = clusterNodes(csr);
  const seed = seedLayout(n, csr, clusters, rank, folderKey);
  const xy = seed.xy;
  scope.postMessage({ type: 'ready', rank, cluster: clusters.cluster, count: clusters.count, sizes: clusters.sizes, xy: xy.slice(), centers: seed.centers, radii: seed.radii, ms: performance.now() - started } satisfies WorkerOut);

  // ForceAtlas2 only moves connected notes; orphans stay in their outer band.
  const live: number[] = [];
  const slot = new Int32Array(n).fill(-1);
  for (let v = 0; v < n; v++) if (csr.offsets[v + 1]! > csr.offsets[v]!) { slot[v] = live.length; live.push(v); }
  const nc = live.length;
  // Above ~20k connected notes the community-packed seed is the layout: a force pass would cost seconds and a hairball is not more readable.
  if (nc < 2 || !src.length || nc > 20000) { scope.postMessage({ type: 'frame', xy, iteration: 0, done: true } satisfies WorkerOut); return; }
  const PPN = 10, PPE = 3;
  const nodes = new Float32Array(nc * PPN), edges = new Float32Array(src.length * PPE);
  live.forEach((v, i) => {
    const o = i * PPN;
    nodes[o] = xy[v * 2]!; nodes[o + 1] = xy[v * 2 + 1]!; nodes[o + 6] = 1; nodes[o + 7] = 1; nodes[o + 8] = 1;
  });
  src.forEach((s, i) => {
    const a = slot[s]! * PPN, b = slot[dst[i]!]! * PPN, o = i * PPE;
    // Links inside a community pull harder than links between them, which is what separates clusters into islands.
    const w = clusters.cluster[s] === clusters.cluster[dst[i]!] ? 1 : 0.3;
    edges[o] = a; edges[o + 1] = b; edges[o + 2] = w;
    nodes[a + 6]! += w; nodes[b + 6]! += w;
  });
  const settings = { linLogMode: true, outboundAttractionDistribution: false, adjustSizes: false, edgeWeightInfluence: 1, scalingRatio: 4, strongGravityMode: false, gravity: 0.6, slowDown: 1 + Math.log(nc), barnesHutOptimize: nc > 500, barnesHutTheta: 0.8 };
  const maxIterations = nc < 1500 ? 300 : nc < 15000 ? 160 : 70;
  const budget = nc < 1500 ? 4000 : 8000;
  let iteration = 0, lastPost = 0, before = new Float32Array(nc * 2);
  const orphans = Array.from({ length: n }, (_, v) => v).filter(v => slot[v]! < 0).sort((a, b) => folderKey[a]! - folderKey[b]! || a - b);
  const publish = (done: boolean) => {
    let reach = 0;
    for (let i = 0; i < nc; i++) { const x = nodes[i * PPN]!, y = nodes[i * PPN + 1]!; xy[live[i]! * 2] = x; xy[live[i]! * 2 + 1] = y; reach = Math.max(reach, Math.hypot(x, y)); }
    placeOrphans(xy, orphans, reach);
    scope.postMessage({ type: 'frame', xy: xy.slice(), iteration, done } satisfies WorkerOut);
    lastPost = performance.now();
  };
  const channel = new MessageChannel();
  const tick = () => {
    if (stopped) return;
    const slice = performance.now();
    while (performance.now() - slice < 12 && iteration < maxIterations) { iterate(settings, nodes, edges); iteration++; }
    let settled = false;
    if (iteration % 10 < 4 && iteration >= 20) {
      let moved = 0;
      for (let i = 0; i < nc; i++) moved += Math.abs(nodes[i * PPN]! - before[i * 2]!) + Math.abs(nodes[i * PPN + 1]! - before[i * 2 + 1]!);
      settled = moved / nc < 0.05 && before[0] !== 0;
      for (let i = 0; i < nc; i++) { before[i * 2] = nodes[i * PPN]!; before[i * 2 + 1] = nodes[i * PPN + 1]!; }
    }
    const done = settled || iteration >= maxIterations || performance.now() - started > budget;
    if (done) { publish(true); return; }
    if (performance.now() - lastPost > 33) publish(false);
    channel.port2.postMessage(0);
  };
  channel.port1.onmessage = tick;
  channel.port2.postMessage(0);
}
