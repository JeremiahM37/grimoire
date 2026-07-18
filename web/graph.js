/**
 * Graph view — a force-directed map of every resolved [[link]] in the vault.
 * Vanilla canvas, no physics library: deterministic phyllotaxis seeding plus a
 * few dozen relaxation iterations per frame. The current note is highlighted;
 * clicking a node navigates.
 */
import { $, api } from "/util.js";

/* Injected by the app shell: */
let openNoteFn = () => {};
let currentPathFn = () => null;

export function initGraph({ openNote, currentPath }) {
  openNoteFn = openNote;
  currentPathFn = currentPath;
}

/* ---------- graph view (canvas force-directed, no deps) ---------- */
let graphAnim = null;
$("#graph-open").onclick = openGraph;
$("#graph-close").onclick = closeGraph;
$("#graph-modal").onclick = (e) => { if (e.target.id === "graph-modal") closeGraph(); };
function closeGraph() {
  $("#graph-modal").classList.add("hidden");
  if (graphAnim) { cancelAnimationFrame(graphAnim); graphAnim = null; }
}
export async function openGraph() {
  const g = await api("/graph");
  $("#graph-modal").classList.remove("hidden");
  const cv = $("#graph-canvas"), ctx = cv.getContext("2d");
  const dpr = devicePixelRatio || 1;
  const fit = () => {
    const r = cv.getBoundingClientRect();
    cv.width = r.width * dpr; cv.height = r.height * dpr;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    return { w: r.width, h: r.height };
  };
  let { w, h } = fit();
  const idx = new Map(g.nodes.map((n, i) => [n.id, i]));
  const deg = new Map();
  for (const e of g.edges) { deg.set(e.src, (deg.get(e.src) || 0) + 1); deg.set(e.dst, (deg.get(e.dst) || 0) + 1); }
  // deterministic seed positions (no Math.random — spread on a phyllotaxis spiral)
  const N = g.nodes.length || 1;
  const nodes = g.nodes.map((n, i) => {
    const a = i * 2.399963, rad = 10 + 16 * Math.sqrt(i);
    return { ...n, x: w / 2 + rad * Math.cos(a), y: h / 2 + rad * Math.sin(a),
             vx: 0, vy: 0, d: deg.get(n.id) || 0 };
  });
  const edges = g.edges.filter((e) => idx.has(e.src) && idx.has(e.dst))
    .map((e) => [idx.get(e.src), idx.get(e.dst)]);
  $("#graph-stat").textContent = `${nodes.length} notes · ${edges.length} links`;

  let alpha = 1;
  const step = () => {
    alpha *= 0.985;
    const k = 0.9;
    // repulsion (O(n²) — fine for a personal vault)
    for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) {
      const a = nodes[i], b = nodes[j];
      let dx = a.x - b.x, dy = a.y - b.y, d2 = dx * dx + dy * dy || 0.01;
      const f = (2600 * alpha) / d2, d = Math.sqrt(d2);
      dx /= d; dy /= d; a.vx += dx * f; a.vy += dy * f; b.vx -= dx * f; b.vy -= dy * f;
    }
    // spring attraction along links
    for (const [i, j] of edges) {
      const a = nodes[i], b = nodes[j];
      let dx = b.x - a.x, dy = b.y - a.y, d = Math.hypot(dx, dy) || 0.01;
      const f = (d - 70) * 0.02 * alpha; dx /= d; dy /= d;
      a.vx += dx * f; a.vy += dy * f; b.vx -= dx * f; b.vy -= dy * f;
    }
    // gravity to center + integrate
    for (const n of nodes) {
      n.vx += (w / 2 - n.x) * 0.002 * alpha; n.vy += (h / 2 - n.y) * 0.002 * alpha;
      n.x += n.vx * k; n.y += n.vy * k; n.vx *= 0.85; n.vy *= 0.85;
      n.x = Math.max(14, Math.min(w - 14, n.x)); n.y = Math.max(14, Math.min(h - 14, n.y));
    }
    draw();
    if (alpha > 0.02) graphAnim = requestAnimationFrame(step);
  };
  const cssVar = (v) => getComputedStyle(document.body).getPropertyValue(v).trim();
  function draw() {
    ctx.clearRect(0, 0, w, h);
    ctx.strokeStyle = cssVar("--line"); ctx.lineWidth = 1; ctx.globalAlpha = 0.7;
    for (const [i, j] of edges) { ctx.beginPath(); ctx.moveTo(nodes[i].x, nodes[i].y); ctx.lineTo(nodes[j].x, nodes[j].y); ctx.stroke(); }
    ctx.globalAlpha = 1;
    for (const n of nodes) {
      const r = 4 + Math.min(9, n.d * 1.6);
      const active = n.id === currentPathFn();
      ctx.beginPath(); ctx.arc(n.x, n.y, r, 0, 7);
      ctx.fillStyle = active ? cssVar("--accent") : cssVar("--link"); ctx.fill();
      if (r >= 7 || active) {
        ctx.fillStyle = cssVar("--ink"); ctx.font = "12px var(--sans)"; ctx.textAlign = "center";
        ctx.fillText((n.title || n.id).slice(0, 22), n.x, n.y - r - 4);
      }
    }
  }
  cv.onclick = (ev) => {
    const r = cv.getBoundingClientRect(), mx = ev.clientX - r.left, my = ev.clientY - r.top;
    let hit = null, best = 400;
    for (const n of nodes) { const d = (n.x - mx) ** 2 + (n.y - my) ** 2; if (d < best) { best = d; hit = n; } }
    if (hit) { closeGraph(); openNoteFn(hit.id); }
  };
  addEventListener("resize", () => { if (!$("#graph-modal").classList.contains("hidden")) ({ w, h } = fit()); }, { once: true });
  draw();   // paint an initial frame immediately (before the first rAF tick)
  step();
}

