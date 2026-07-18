/**
 * Grimoire live editor — the entry point bundled into web/vendor/editor.js.
 *
 * Exposes a single factory, `createLiveEditor`, that mounts a CodeMirror 6
 * editor with live-preview editing:
 *
 *  - markdown is styled in place (headings sized, bold bold, links linked)
 *  - the raw markup (##, **, [[]]) is hidden except where you're editing
 *  - task checkboxes are real, clickable checkboxes
 *  - [[wiki-links]] are clickable; `[[`, `#` and `/` trigger completions
 *
 * The app supplies data + behavior through callbacks so this bundle stays a
 * pure editor: it knows nothing about Grimoire's API or state.
 */
import { EditorState, StateEffect } from "@codemirror/state";
import {
  EditorView, keymap, drawSelection, highlightActiveLine, placeholder,
  Decoration, ViewPlugin, WidgetType, lineNumbers,
} from "@codemirror/view";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { markdown, markdownLanguage, insertNewlineContinueMarkup } from "@codemirror/lang-markdown";
import { syntaxTree, indentUnit, foldGutter, foldKeymap } from "@codemirror/language";
import { autocompletion, startCompletion, closeBrackets, closeBracketsKeymap } from "@codemirror/autocomplete";
import { searchKeymap, highlightSelectionMatches, openSearchPanel } from "@codemirror/search";

/* ------------------------------------------------------------------ regexes */

const WIKILINK = /\[\[([^\[\]|]+?)(?:\|([^\[\]]+))?\]\]/g;
const TAG = /(^|\s)#([A-Za-z][\w/-]*)/g;
const HIGHLIGHT = /==([^=\n]+)==/g;

/* ------------------------------------------------------------- widget types */

class CheckboxWidget extends WidgetType {
  constructor(checked) { super(); this.checked = checked; }
  eq(other) { return other.checked === this.checked; }
  toDOM() {
    const box = document.createElement("input");
    box.type = "checkbox";
    box.className = "gr-task-box";
    box.checked = this.checked;
    return box;
  }
  ignoreEvent() { return false; }   // let clicks reach our mousedown handler
}

class HrWidget extends WidgetType {
  toDOM() { const hr = document.createElement("hr"); hr.className = "gr-hr"; return hr; }
}

class ImageWidget extends WidgetType {
  constructor(src, alt) { super(); this.src = src; this.alt = alt; }
  eq(other) { return other.src === this.src; }
  toDOM() {
    const img = document.createElement("img");
    img.className = "gr-img";
    img.src = this.src;
    img.alt = this.alt;
    img.loading = "lazy";
    return img;
  }
  ignoreEvent() { return true; }
}

const IMAGE_EMBED = /!\[\[([^\[\]|]+?\.(?:png|jpe?g|gif|webp|svg|avif))\]\]/gi;

/* ------------------------------------------------- live-preview decorations */

/**
 * Build the full decoration set for the visible viewport.
 *
 * "Active" means the primary selection touches the element — active elements
 * show their raw markup for editing; inactive ones render clean.
 */
function buildDecorations(view) {
  const widgets = [];
  const { from: selFrom, to: selTo } = view.state.selection.main;
  const touches = (from, to) => selFrom <= to && selTo >= from;
  const doc = view.state.doc;

  for (const { from, to } of view.visibleRanges) {
    // --- syntax-tree driven decorations (headings, emphasis, code, quotes) ---
    syntaxTree(view.state).iterate({
      from, to,
      enter: (node) => {
        const name = node.name;
        if (name.startsWith("ATXHeading")) {
          const level = +name.slice(10) || 1;
          const line = doc.lineAt(node.from);
          widgets.push(Decoration.line({ class: `gr-h gr-h${level}` }).range(line.from));
        } else if (name === "HeaderMark") {
          const line = doc.lineAt(node.from);
          if (!touches(line.from, line.to)) {
            // hide "## " (mark + the following space)
            widgets.push(Decoration.replace({}).range(node.from, Math.min(node.to + 1, line.to)));
          }
        } else if (name === "StrongEmphasis") {
          widgets.push(Decoration.mark({ class: "gr-strong" }).range(node.from, node.to));
        } else if (name === "Emphasis") {
          widgets.push(Decoration.mark({ class: "gr-em" }).range(node.from, node.to));
        } else if (name === "Strikethrough") {
          widgets.push(Decoration.mark({ class: "gr-strike" }).range(node.from, node.to));
        } else if (name === "EmphasisMark" || name === "StrikethroughMark") {
          if (!touches(node.from - 1, node.to + 1))
            widgets.push(Decoration.replace({}).range(node.from, node.to));
        } else if (name === "InlineCode") {
          widgets.push(Decoration.mark({ class: "gr-code" }).range(node.from, node.to));
        } else if (name === "CodeMark") {
          const parent = node.node.parent;
          if (parent?.name === "InlineCode" || parent?.name === "FencedCode") {
            if (parent.name === "InlineCode" && !touches(parent.from, parent.to))
              widgets.push(Decoration.replace({}).range(node.from, node.to));
          }
        } else if (name === "FencedCode") {
          for (let l = doc.lineAt(node.from).number; l <= doc.lineAt(node.to).number; l++)
            widgets.push(Decoration.line({ class: "gr-codeblock" }).range(doc.line(l).from));
        } else if (name === "Blockquote") {
          for (let l = doc.lineAt(node.from).number; l <= doc.lineAt(node.to).number; l++)
            widgets.push(Decoration.line({ class: "gr-quote" }).range(doc.line(l).from));
        } else if (name === "TaskMarker") {
          const line = doc.lineAt(node.from);
          const checked = /x/i.test(doc.sliceString(node.from, node.to));
          if (!touches(line.from, line.to)) {
            // swallow the leading "- " too — a task renders as just a checkbox
            const lead = line.text.match(/^(\s*)[-*]\s+/);
            const from = lead ? line.from + lead[1].length : node.from;
            widgets.push(Decoration.replace({ widget: new CheckboxWidget(checked) })
              .range(from, node.to));
            if (checked)
              widgets.push(Decoration.mark({ class: "gr-task-done" }).range(node.to, line.to));
          }
        } else if (name === "HorizontalRule") {
          const line = doc.lineAt(node.from);
          if (!touches(line.from, line.to))
            widgets.push(Decoration.replace({ widget: new HrWidget(), block: false })
              .range(node.from, node.to));
        } else if (name === "URL") {
          widgets.push(Decoration.mark({ class: "gr-url" }).range(node.from, node.to));
        }
      },
    });

    // --- regex-driven decorations (wiki-links, tags, ==highlight==) ---
    const text = doc.sliceString(from, to);
    for (const m of text.matchAll(WIKILINK)) {
      const s = from + m.index, e = s + m[0].length;
      const active = touches(s, e);
      const target = m[1].trim();
      widgets.push(Decoration.mark({
        class: "gr-wikilink", attributes: { "data-target": target.split("#")[0] },
      }).range(s, e));
      if (!active) {
        // collapse "[[Target|alias]]" to just its label
        const label = (m[2] || m[1]).trim();
        const labelStart = m[2] ? s + 2 + m[1].length + 1 : s + 2;
        widgets.push(Decoration.replace({}).range(s, labelStart));          // "[[" (+target|)
        widgets.push(Decoration.replace({}).range(labelStart + label.length, e)); // "]]"
      }
    }
    for (const m of text.matchAll(IMAGE_EMBED)) {
      const s = from + m.index, e = s + m[0].length;
      if (!touches(s, e)) {
        // swap the raw embed text for the actual image (click to edit reveals it)
        widgets.push(Decoration.replace({
          widget: new ImageWidget("/api/file/" + encodeURI(m[1].trim()), m[1].trim()),
        }).range(s, e));
      }
    }
    for (const m of text.matchAll(TAG)) {
      const s = from + m.index + m[1].length, e = s + 1 + m[2].length;
      widgets.push(Decoration.mark({
        class: "gr-tag", attributes: { "data-tag": m[2] },
      }).range(s, e));
    }
    for (const m of text.matchAll(HIGHLIGHT)) {
      const s = from + m.index, e = s + m[0].length;
      widgets.push(Decoration.mark({ class: "gr-mark" }).range(s, e));
      if (!touches(s, e)) {
        widgets.push(Decoration.replace({}).range(s, s + 2));
        widgets.push(Decoration.replace({}).range(e - 2, e));
      }
    }
  }
  // CM requires sorted ranges; line decorations must come before point ones at
  // the same position — sort by from, then startSide.
  return Decoration.set(widgets.map((w) => w), true);
}

const livePreview = ViewPlugin.fromClass(class {
  constructor(view) { this.decorations = buildDecorations(view); }
  update(update) {
    if (update.docChanged || update.selectionSet || update.viewportChanged)
      this.decorations = buildDecorations(update.view);
  }
}, { decorations: (v) => v.decorations });

/* ------------------------------------------------------------- interactions */

/** Click handling: wiki-links open notes, tags filter, checkboxes toggle. */
function interactions(callbacks) {
  return EditorView.domEventHandlers({
    paste(event, view) {
      const files = [...(event.clipboardData?.files || [])];
      if (files.length && callbacks.onFiles) {
        event.preventDefault();
        callbacks.onFiles(files);
        return true;
      }
      // pasting a URL over selected text turns it into a markdown link
      const text = event.clipboardData?.getData("text/plain") || "";
      const { from, to } = view.state.selection.main;
      if (from !== to && /^https?:\/\/\S+$/.test(text.trim())) {
        event.preventDefault();
        const label = view.state.sliceDoc(from, to);
        view.dispatch({ changes: { from, to, insert: `[${label}](${text.trim()})` } });
        return true;
      }
      return false;
    },
    drop(event, view) {
      const files = [...(event.dataTransfer?.files || [])];
      if (files.length && callbacks.onFiles) {
        event.preventDefault();
        const pos = view.posAtCoords({ x: event.clientX, y: event.clientY });
        if (pos != null) view.dispatch({ selection: { anchor: pos } });
        callbacks.onFiles(files);
        return true;
      }
      return false;
    },
    mousedown(event, view) {
      const link = event.target.closest?.(".gr-wikilink");
      if (link && !event.shiftKey) {
        event.preventDefault();
        callbacks.onOpenLink?.(link.dataset.target, { split: event.ctrlKey || event.metaKey });
        return true;
      }
      const tag = event.target.closest?.(".gr-tag");
      if (tag && (event.ctrlKey || event.metaKey)) {
        event.preventDefault();
        callbacks.onTagClick?.(tag.dataset.tag);
        return true;
      }
      if (event.target.classList?.contains("gr-task-box")) {
        event.preventDefault();
        const pos = view.posAtDOM(event.target);
        const line = view.state.doc.lineAt(pos);
        const updated = line.text.replace(/^(\s*[-*]\s+)\[([ xX])\]/,
          (_, pre, mark) => pre + (/x/i.test(mark) ? "[ ]" : "[x]"));
        if (updated !== line.text)
          view.dispatch({ changes: { from: line.from, to: line.to, insert: updated } });
        return true;
      }
      return false;
    },
  });
}

/* ------------------------------------------------------------- autocomplete */

/**
 * One completion source covering Grimoire's three triggers:
 *   [[…  → note links       #…  → tags       /… at line start → commands
 * The host app supplies the data; `apply` on commands runs them.
 */
function grimoireCompletions(callbacks) {
  return (context) => {
    const line = context.state.doc.lineAt(context.pos);
    const before = context.state.sliceDoc(line.from, context.pos);

    const link = before.match(/\[\[([^\[\]]*)$/);
    if (link) {
      const items = callbacks.getLinkCompletions?.(link[1]) || [];
      return {
        from: context.pos - link[1].length,
        options: items.map((t) => ({
          label: t, type: "class",
          // closeBrackets may already have inserted the closing "]]" — reuse it
          // instead of appending a second pair (the stray-"]]" bug)
          apply: (view, _c, from, to) => {
            const after = view.state.sliceDoc(to, to + 2);
            const closed = after === "]]";
            view.dispatch({
              changes: { from, to, insert: closed ? t : `${t}]]` },
              selection: { anchor: from + t.length + 2 },
            });
          },
        })),
        validFor: /^[^\[\]]*$/,
      };
    }
    const tag = before.match(/(?:^|\s)#([\w/-]*)$/);
    if (tag) {
      const items = callbacks.getTagCompletions?.(tag[1]) || [];
      return {
        from: context.pos - tag[1].length,
        options: items.map((t) => ({ label: t, type: "keyword" })),
        validFor: /^[\w/-]*$/,
      };
    }
    const slash = before.match(/^\s*\/([\w-]*)$/);
    if (slash) {
      const items = callbacks.getSlashCommands?.(slash[1]) || [];
      return {
        from: line.from + before.indexOf("/"),
        options: items.map((cmd) => ({
          label: `/${cmd.name}`, detail: cmd.detail || "", type: "function",
          apply: (view, _completion, from, to) => {
            if (cmd.insert !== undefined) {
              view.dispatch({ changes: { from, to, insert: cmd.insert } });
            } else {
              view.dispatch({ changes: { from, to, insert: "" } });
              cmd.run?.();
            }
          },
        })),
        validFor: /^[\w-]*$/,
      };
    }
    return null;
  };
}

/* ------------------------------------------------------------------ factory */

export function createLiveEditor({ parent, doc = "", callbacks = {} }) {
  const updateListener = EditorView.updateListener.of((update) => {
    if (update.docChanged) callbacks.onChange?.(update.state.doc.toString());
  });

  const state = EditorState.create({
    doc,
    extensions: [
      history(),
      drawSelection(),
      highlightActiveLine(),
      highlightSelectionMatches(),
      indentUnit.of("  "),
      foldGutter({ openText: "▾", closedText: "▸" }),   // fold headings/sections
      EditorView.lineWrapping,
      placeholder("Start writing…"),
      markdown({ base: markdownLanguage }),
      closeBrackets(),
      livePreview,
      interactions(callbacks),
      autocompletion({
        override: [grimoireCompletions(callbacks)],
        activateOnTyping: true, icons: false,
      }),
      keymap.of([
        ...closeBracketsKeymap,
        { key: "Enter", run: insertNewlineContinueMarkup },
        { key: "Mod-s", run: () => { callbacks.onSave?.(); return true; } },
        indentWithTab,
        ...historyKeymap,
        ...searchKeymap,
        ...foldKeymap,
        ...defaultKeymap,
      ]),
      updateListener,
    ],
  });

  const view = new EditorView({ state, parent });

  /* Small, stable adapter surface — everything app.js needs, nothing more. */
  return {
    view,
    getValue: () => view.state.doc.toString(),
    setValue: (text) => view.dispatch({
      changes: { from: 0, to: view.state.doc.length, insert: text },
    }),
    getSelection: () => {
      const { from, to } = view.state.selection.main;
      return { from, to, text: view.state.sliceDoc(from, to) };
    },
    replaceSelection: (text, selectInserted = false) => {
      const { from, to } = view.state.selection.main;
      view.dispatch({
        changes: { from, to, insert: text },
        selection: selectInserted
          ? { anchor: from, head: from + text.length }
          : { anchor: from + text.length },
      });
      view.focus();
    },
    setCursor: (pos) => view.dispatch({ selection: { anchor: pos }, scrollIntoView: true }),
    focus: () => view.focus(),
    openCompletion: () => startCompletion(view),
    destroy: () => view.destroy(),
  };
}

export { EditorView, lineNumbers, StateEffect, openSearchPanel };
