/**
 * Mermaid diagrams for Grimoire — ```mermaid fences render as diagrams in the
 * preview. The vendored bundle (~2.6 MB) is lazy-loaded on first use only.
 */
let mermaidReady = null;
let seq = 0;

function ensureMermaid(grimoire) {
  if (!mermaidReady) {
    mermaidReady = grimoire.loadScript("vendor/mermaid.min.js").then(() => {
      const dark = matchMedia("(prefers-color-scheme: dark)").matches
        || document.documentElement.dataset.theme === "dark";
      window.mermaid.initialize({ startOnLoad: false, theme: dark ? "dark" : "neutral" });
    });
  }
  return mermaidReady;
}

export async function activate(grimoire) {
  grimoire.registerFenceRenderer("mermaid", async (el, source) => {
    await ensureMermaid(grimoire);
    try {
      const { svg } = await window.mermaid.render(`gr-mermaid-${seq++}`, source.trim());
      el.innerHTML = svg;
    } catch (e) {
      el.textContent = `mermaid: ${e.message || e}`;
    }
  });

  grimoire.registerSlashSnippet({
    name: "mermaid", detail: "diagram block",
    insert: "```mermaid\nflowchart LR\n  A --> B\n```\n",
  });
}
