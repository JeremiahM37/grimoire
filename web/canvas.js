/**
 * Canvas view — a minimal-but-real visual board over the JSON Canvas format
 * (https://jsoncanvas.org), interoperable with other JSON Canvas apps.
 *
 * Interactions:
 *   drag background → pan            wheel → zoom
 *   drag node       → move           double-click background → new text card
 *   double-click card → edit text    shift-drag card → connect (edge)
 *   click card (select) + Delete → remove       ⌘/Ctrl-click file card → open note
 *
 * Rendering: HTML cards absolutely positioned inside a transformed layer, SVG
 * underneath for edges. Saves are debounced PUTs of the whole document.
 */

let host = null;   // injected: { api, toast, openNote, esc }

export function initCanvas(hostApi) { host = hostApi; }

export async function openCanvasPicker() {
  const list = await host.api("/canvas");
  if (!list.length) return createCanvas();
  const name = prompt(
    "Open canvas:\n" + list.map((c) => `• ${c.name}`).join("\n") +
    "\n\nType a name (or a new name to create):", list[0].name);
  if (!name) return;
  const hit = list.find((c) => c.name.toLowerCase() === name.toLowerCase());
  if (hit) return openCanvas(hit.path);
  const made = await host.api("/canvas", { method: "POST", body: { name } });
  openCanvas(made.path);
}

export async function createCanvas() {
  const name = prompt("New canvas name:");
  if (!name) return;
  try {
    const made = await host.api("/canvas", { method: "POST", body: { name } });
    openCanvas(made.path);
  } catch (e) { host.toast(e.message, true); }
}

export async function openCanvas(path) {
  const doc = await host.api(`/canvas/${encodeURI(path)}`);
  const overlay = document.createElement("div");
  overlay.id = "canvas-view";
  overlay.innerHTML = `
    <header class="cv-head">
      <b>🗺 ${host.esc(doc.path.replace(/^canvases\//, "").replace(/\.canvas$/, ""))}</b>
      <span class="cv-hint">double-click: new card · shift-drag: connect · del: remove</span>
      <span class="cv-save" id="cv-save"></span>
      <button class="icon" id="cv-close">✕</button>
    </header>
    <div class="cv-viewport">
      <svg class="cv-edges"></svg>
      <div class="cv-layer"></div>
    </div>`;
  document.body.appendChild(overlay);

  const viewport = overlay.querySelector(".cv-viewport");
  const layer = overlay.querySelector(".cv-layer");
  const svg = overlay.querySelector(".cv-edges");
  const view = { x: 60, y: 60, zoom: 1 };
  let selected = null;
  let saveTimer = null;

  const nextId = () => `n${Date.now().toString(36)}${Math.floor(performance.now() % 1000)}`;

  function scheduleSave() {
    overlay.querySelector("#cv-save").textContent = "…";
    clearTimeout(saveTimer);
    saveTimer = setTimeout(async () => {
      try {
        await host.api(`/canvas/${encodeURI(doc.path)}`, {
          method: "PUT", body: { nodes: doc.nodes, edges: doc.edges } });
        overlay.querySelector("#cv-save").textContent = "saved";
        setTimeout(() => (overlay.querySelector("#cv-save").textContent = ""), 1200);
      } catch (e) { host.toast(`canvas save failed: ${e.message}`, true); }
    }, 600);
  }

  function applyView() {
    layer.style.transform = `translate(${view.x}px, ${view.y}px) scale(${view.zoom})`;
    drawEdges();
  }

  function center(n) {
    return { x: n.x + (n.width || 200) / 2, y: n.y + (n.height || 80) / 2 };
  }

  function drawEdges() {
    const w = viewport.clientWidth, h = viewport.clientHeight;
    svg.setAttribute("viewBox", `0 0 ${w} ${h}`);
    svg.innerHTML = doc.edges.map((e) => {
      const a = doc.nodes.find((n) => n.id === e.fromNode);
      const b = doc.nodes.find((n) => n.id === e.toNode);
      if (!a || !b) return "";
      const p1 = center(a), p2 = center(b);
      const sx = p1.x * view.zoom + view.x, sy = p1.y * view.zoom + view.y;
      const ex = p2.x * view.zoom + view.x, ey = p2.y * view.zoom + view.y;
      return `<path d="M ${sx} ${sy} C ${(sx + ex) / 2} ${sy}, ${(sx + ex) / 2} ${ey}, ${ex} ${ey}"
        class="cv-edge" data-id="${e.id}"/>`;
    }).join("");
  }

  function renderNodes() {
    layer.innerHTML = "";
    for (const n of doc.nodes) {
      const el = document.createElement("div");
      el.className = `cv-node cv-${n.type || "text"}` + (selected === n.id ? " sel" : "");
      el.style.cssText = `left:${n.x}px; top:${n.y}px; width:${n.width || 200}px; min-height:${n.height || 80}px`;
      el.dataset.id = n.id;
      if (n.type === "file")
        el.innerHTML = `<div class="cv-file">🗒 ${host.esc((n.file || "").replace(/\.md$/, ""))}</div>`;
      else
        el.textContent = n.text || "";
      wireNode(el, n);
      layer.appendChild(el);
    }
    drawEdges();
  }

  function wireNode(el, n) {
    el.onpointerdown = (ev) => {
      ev.stopPropagation();
      selected = n.id; renderNodes();
      const el2 = layer.querySelector(`[data-id="${n.id}"]`);
      const startX = ev.clientX, startY = ev.clientY, ox = n.x, oy = n.y;
      if (ev.shiftKey) {          // shift-drag → connect to the drop target
        const onUp = (up) => {
          const t = document.elementFromPoint(up.clientX, up.clientY)?.closest?.(".cv-node");
          if (t && t.dataset.id !== n.id) {
            doc.edges.push({ id: nextId(), fromNode: n.id, toNode: t.dataset.id });
            scheduleSave();
          }
          renderNodes();
          removeEventListener("pointerup", onUp);
        };
        addEventListener("pointerup", onUp);
        return;
      }
      const onMove = (mv) => {
        n.x = ox + (mv.clientX - startX) / view.zoom;
        n.y = oy + (mv.clientY - startY) / view.zoom;
        el2.style.left = `${n.x}px`; el2.style.top = `${n.y}px`;
        drawEdges();
      };
      const onUp = () => {
        removeEventListener("pointermove", onMove);
        removeEventListener("pointerup", onUp);
        scheduleSave();
      };
      addEventListener("pointermove", onMove);
      addEventListener("pointerup", onUp);
    };
    el.ondblclick = (ev) => {
      ev.stopPropagation();
      if (n.type === "file") return host.openNote(n.file);
      const text = prompt("Card text:", n.text || "");
      if (text !== null) { n.text = text; renderNodes(); scheduleSave(); }
    };
    if (n.type === "file")
      el.onclick = (ev) => { if (ev.ctrlKey || ev.metaKey) host.openNote(n.file); };
  }

  /* background: pan, zoom, create, delete, close */
  viewport.onpointerdown = (ev) => {
    if (ev.target.closest(".cv-node")) return;
    selected = null; renderNodes();
    const sx = ev.clientX, sy = ev.clientY, ox = view.x, oy = view.y;
    const onMove = (mv) => { view.x = ox + mv.clientX - sx; view.y = oy + mv.clientY - sy; applyView(); };
    const onUp = () => { removeEventListener("pointermove", onMove); removeEventListener("pointerup", onUp); };
    addEventListener("pointermove", onMove);
    addEventListener("pointerup", onUp);
  };
  viewport.onwheel = (ev) => {
    ev.preventDefault();
    const factor = ev.deltaY < 0 ? 1.1 : 0.9;
    view.zoom = Math.max(0.2, Math.min(2.5, view.zoom * factor));
    applyView();
  };
  viewport.ondblclick = (ev) => {
    if (ev.target.closest(".cv-node")) return;
    const x = (ev.clientX - view.x) / view.zoom, y = (ev.clientY - view.y) / view.zoom;
    const text = prompt("Card text (or [[Note Title]] to embed a note):");
    if (!text) return;
    const wiki = text.match(/^\[\[(.+?)\]\]$/);
    doc.nodes.push(wiki
      ? { id: nextId(), type: "file", file: `${wiki[1]}.md`, x, y, width: 220, height: 60 }
      : { id: nextId(), type: "text", text, x, y, width: 220, height: 80 });
    renderNodes(); scheduleSave();
  };
  const onKey = (ev) => {
    if (ev.key === "Escape") return close();
    if ((ev.key === "Delete" || ev.key === "Backspace") && selected
        && !ev.target.closest("input, textarea")) {
      doc.nodes = doc.nodes.filter((n) => n.id !== selected);
      doc.edges = doc.edges.filter((e) => e.fromNode !== selected && e.toNode !== selected);
      selected = null; renderNodes(); scheduleSave();
    }
  };
  function close() {
    removeEventListener("keydown", onKey, true);
    overlay.remove();
  }
  addEventListener("keydown", onKey, true);
  overlay.querySelector("#cv-close").onclick = close;

  renderNodes(); applyView();
}
