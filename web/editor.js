/**
 * Editor facade — one stable surface over the two editing modes:
 *
 *  - "live"    CodeMirror 6 live preview (vendor/editor.js, lazy-loaded)
 *  - "classic" the plain <textarea> (zero-dependency fallback)
 *
 * The hidden textarea (#content) remains the mirror of the document in live
 * mode: every CM change is copied into it, so the rest of the app can keep
 * *reading* `ta.value` unchanged. Anything that *writes* the textarea calls
 * `Editor.sync()` afterwards to push the new text into CM.
 *
 * Mode is a client concern (per-device), persisted in localStorage.
 */

const MODE_KEY = "grimoire-editor-mode";

export const Editor = {
  live: null,          // adapter returned by createLiveEditor, when active
  hooks: {},           // app-provided callbacks (completions, open-link, save…)
  _ta: null,

  get mode() { return localStorage.getItem(MODE_KEY) || "live"; },
  set mode(m) { localStorage.setItem(MODE_KEY, m); },
  get isLive() { return !!this.live; },

  /** Wire the facade. Called once at startup; mounts CM when mode is "live". */
  async init(hooks) {
    this.hooks = hooks;
    this._ta = document.querySelector("#content");
    if (this.mode !== "live") return;
    try {
      const { createLiveEditor } = await import("/vendor/editor.js");
      const host = document.querySelector("#live-editor");
      this.live = createLiveEditor({
        parent: host,
        doc: this._ta.value,
        callbacks: {
          onChange: (text) => { this._ta.value = text; hooks.onInput?.(); },
          onSave: () => hooks.onSave?.(),
          onOpenLink: (target, opts) => hooks.onOpenLink?.(target, opts),
          onTagClick: (tag) => hooks.onTagClick?.(tag),
          getLinkCompletions: (q) => hooks.getLinkCompletions?.(q) || [],
          getTagCompletions: (q) => hooks.getTagCompletions?.(q) || [],
          getSlashCommands: (q) => hooks.getSlashCommands?.(q) || [],
          onFiles: (files) => hooks.onFiles?.(files),
        },
      });
      document.body.classList.add("live-editor-on");
    } catch (e) {
      // vendor bundle missing/broken → degrade to classic, loudly in console
      console.error("live editor failed to load, falling back to classic:", e);
      this.live = null;
      document.body.classList.remove("live-editor-on");
    }
  },

  /** Switch modes (settings UI). Reloads the page — cheapest correct swap. */
  setMode(m) { this.mode = m; location.reload(); },

  /** Push the textarea's current value into CM after an external write. */
  sync() {
    if (this.live && this.live.getValue() !== this._ta.value)
      this.live.setValue(this._ta.value);
  },

  /** Editable state (locked/encrypted notes are read-only). */
  setReadOnly(ro) {
    if (this.live) this.live.view.contentDOM.contentEditable = ro ? "false" : "true";
  },

  focus() { this.live ? this.live.focus() : this._ta.focus(); },

  /* --- editing operations (used by the toolbar + palette commands) --- */

  surround(pre, post = pre, placeholder = "") {
    if (!this.live) return false;                 // classic path handles itself
    const { text } = this.live.getSelection();
    const sel = text || placeholder;
    this.live.replaceSelection(pre + sel + post, false);
    this.hooks.onInput?.();
    return true;
  },

  prefixLine(prefix) {
    if (!this.live) return false;
    const view = this.live.view;
    const line = view.state.doc.lineAt(view.state.selection.main.head);
    view.dispatch({ changes: { from: line.from, insert: prefix } });
    view.focus();
    this.hooks.onInput?.();
    return true;
  },

  insert(text) {
    if (!this.live) return false;
    this.live.replaceSelection(text, false);
    this.hooks.onInput?.();
    return true;
  },

  /** CM's search panel (live mode's find & replace). */
  async openSearch() {
    if (!this.live) return false;
    const { openSearchPanel } = await import("/vendor/editor.js")
      .then((m) => ({ openSearchPanel: m.openSearchPanel }))
      .catch(() => ({}));
    if (openSearchPanel) { openSearchPanel(this.live.view); return true; }
    return false;
  },
};
