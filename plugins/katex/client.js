/**
 * KaTeX math for Grimoire.
 *
 * - ```math fenced blocks render as display math
 * - $inline$ and $$display$$ in preview text render in place
 *
 * The vendored KaTeX bundle (~280 KB + fonts) is lazy-loaded the first time a
 * page actually contains math, so notes without math pay nothing.
 */
let katexReady = null;

function ensureKatex(grimoire) {
  if (!katexReady) {
    grimoire.loadStyles("vendor/katex.min.css");
    katexReady = grimoire.loadScript("vendor/katex.min.js");
  }
  return katexReady;
}

function renderTo(el, tex, displayMode) {
  try {
    window.katex.render(tex, el, { displayMode, throwOnError: false });
  } catch (e) {
    el.textContent = tex;
  }
}

export async function activate(grimoire) {
  // ```math fences
  grimoire.registerFenceRenderer("math", async (el, source) => {
    await ensureKatex(grimoire);
    renderTo(el, source.trim(), true);
  });

  // $…$ / $$…$$ in rendered preview text
  grimoire.registerPreviewTransform(async (root) => {
    const hasMath = /\$[^$\s][^$]*\$/.test(root.textContent || "");
    if (!hasMath) return;
    await ensureKatex(grimoire);
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
      acceptNode: (n) =>
        n.parentElement.closest("code, pre, .katex")
          ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT,
    });
    const targets = [];
    for (let n = walker.nextNode(); n; n = walker.nextNode())
      if (n.nodeValue.includes("$")) targets.push(n);
    for (const node of targets) {
      const frag = document.createDocumentFragment();
      let rest = node.nodeValue;
      const re = /\$\$([^$]+)\$\$|\$([^$\s][^$]*?)\$/;
      let m;
      while ((m = re.exec(rest))) {
        frag.appendChild(document.createTextNode(rest.slice(0, m.index)));
        const span = document.createElement("span");
        renderTo(span, (m[1] || m[2]).trim(), !!m[1]);
        frag.appendChild(span);
        rest = rest.slice(m.index + m[0].length);
      }
      frag.appendChild(document.createTextNode(rest));
      node.replaceWith(frag);
    }
  });

  grimoire.registerSlashSnippet({
    name: "math", detail: "display math block", insert: "```math\n\n```\n",
  });
}
