/**
 * Client plugin runtime.
 *
 * Each enabled plugin is an ES module served same-origin from
 * /plugins/{name}/{entry} (strict CSP still applies — no external hosts). A
 * plugin exports `activate(grimoire)` (or a default function) and receives the
 * stable API surface below. Everything a plugin registers is additive; a
 * broken plugin logs and is skipped, never taking the app down.
 *
 * The host app injects its internals once via `initPlugins(host)` — plugins
 * never touch app globals directly.
 */

const registry = {
  commands: [],                 // [{icon, name, run}] appended to the palette
  slashSnippets: [],            // [{name, detail, insert}] for the / menu
  fenceRenderers: new Map(),    // lang -> async (el, source) => void
  previewTransforms: [],        // async (rootEl) => void, run after each preview render
  panels: [],                   // [{id, title, render(el)}] sidebar sections
  listeners: new Map(),         // event -> [cb]
  loaded: [],                   // [{name, version}] successfully activated
};

let host = null;                // app internals, injected by initPlugins

/** The API object handed to every plugin's activate(). Keep this small and
 *  stable — it is the public contract documented in docs/PLUGINS.md. */
function makeApi(pluginName) {
  return {
    name: pluginName,

    /* ---- contributions ---- */
    registerCommand(cmd) {
      registry.commands.push({ icon: cmd.icon || "🔌", name: cmd.name, run: cmd.run });
      host?.onCommandsChanged?.();
    },
    registerSlashSnippet(snip) { registry.slashSnippets.push(snip); },
    registerFenceRenderer(lang, render) { registry.fenceRenderers.set(lang, render); },
    registerPreviewTransform(fn) { registry.previewTransforms.push(fn); },
    registerPanel(panel) { registry.panels.push(panel); host?.renderPanels?.(); },

    /* ---- events: "boot" | "note-open" | "note-save" ---- */
    on(event, cb) {
      if (!registry.listeners.has(event)) registry.listeners.set(event, []);
      registry.listeners.get(event).push(cb);
    },

    /* ---- host services ---- */
    api: (...args) => host.api(...args),            // authenticated fetch helper
    toast: (...args) => host.toast(...args),
    openNote: (path) => host.openNote(path),
    getCurrentNote: () => host.getCurrentNote(),    // {path, title, body} | null
    insertText: (text) => host.insertText(text),

    /** Load an extra same-origin asset from this plugin's directory. */
    loadScript(rel) {
      return new Promise((resolve, reject) => {
        const s = document.createElement("script");
        s.src = `/plugins/${pluginName}/${rel}`;
        s.onload = resolve; s.onerror = () => reject(new Error(`load failed: ${rel}`));
        document.head.appendChild(s);
      });
    },
    loadStyles(rel) {
      const l = document.createElement("link");
      l.rel = "stylesheet"; l.href = `/plugins/${pluginName}/${rel}`;
      document.head.appendChild(l);
    },
    assetUrl: (rel) => `/plugins/${pluginName}/${rel}`,
  };
}

export const Plugins = {
  registry,

  /** Fetch the enabled plugin list and activate each one. */
  async init(hostApi) {
    host = hostApi;
    let list = [];
    try { list = await host.api("/plugins"); } catch { return; }
    for (const p of list.filter((p) => p.enabled)) {
      try {
        const mod = await import(p.client_url);
        if (p.styles_url) {
          const l = document.createElement("link");
          l.rel = "stylesheet"; l.href = p.styles_url;
          document.head.appendChild(l);
        }
        const activate = mod.activate || mod.default;
        await activate?.(makeApi(p.name));
        registry.loaded.push({ name: p.name, version: p.version });
      } catch (e) {
        console.error(`plugin "${p.name}" failed to activate:`, e);
      }
    }
    this.emit("boot");
  },

  emit(event, payload) {
    for (const cb of registry.listeners.get(event) || []) {
      try { cb(payload); } catch (e) { console.error(`plugin ${event} handler:`, e); }
    }
  },

  /** Run plugin fence renderers over a rendered preview container. */
  async renderFences(root) {
    for (const code of root.querySelectorAll("pre > code[data-lang]")) {
      const render = registry.fenceRenderers.get(code.dataset.lang);
      if (!render) continue;
      const holder = document.createElement("div");
      holder.className = `fence-plugin fence-${code.dataset.lang}`;
      code.parentElement.replaceWith(holder);
      try { await render(holder, code.textContent); }
      catch (e) { holder.textContent = `plugin render failed: ${e.message}`; }
    }
    for (const fn of registry.previewTransforms) {
      try { await fn(root); }
      catch (e) { console.error("preview transform failed:", e); }
    }
  },
};
