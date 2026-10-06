// Badges on memory lines in the editor (Live Preview and source mode).
//
// The `<!--m id=… -->` trailer is what makes a bullet addressable, and it is
// noise to a person reading the note. When the cursor is elsewhere the trailer
// is folded into a badge saying who wrote the line and whether the server
// treats it as yours; put the cursor on the line and the raw text comes back,
// so editing — which is how a person corrects an agent — is never in the way.

import { RangeSetBuilder } from '@codemirror/state'
import {
  Decoration,
  type DecorationSet,
  EditorView,
  ViewPlugin,
  type ViewUpdate,
  WidgetType,
} from '@codemirror/view'
import type { Badge } from './badge'
import { scanNote, type ScannedLine } from './scan'

export interface EditorHooks {
  enabled(): boolean
  showTrailers(): boolean
  /** Only badge files Grimoire serves; null path means "not a vault file". */
  isGrimoireFile(view: EditorView): boolean
  onBadgeClick(badge: Badge, line: ScannedLine): void
}

class BadgeWidget extends WidgetType {
  constructor(private badge: Badge, private scanned: ScannedLine, private hooks: EditorHooks) {
    super()
  }

  eq(other: BadgeWidget): boolean {
    return other.badge.label === this.badge.label && other.badge.title === this.badge.title &&
      other.badge.kind === this.badge.kind
  }

  toDOM(): HTMLElement {
    return renderBadge(this.badge, () => this.hooks.onBadgeClick(this.badge, this.scanned))
  }

  ignoreEvent(): boolean {
    return false
  }
}

export function renderBadge(badge: Badge, onClick?: () => void): HTMLElement {
  const el = document.createElement('span')
  el.className = `grimoire-badge grimoire-badge-${badge.kind}`
  el.textContent = badge.label
  el.setAttribute('aria-label', badge.title)
  el.setAttribute('data-tooltip-position', 'top')
  el.style.setProperty('--grimoire-hue', String(badge.hue))
  if (onClick) {
    el.addEventListener('mousedown', (ev) => {
      // Keep the click from moving the cursor onto the line, which would
      // swap the badge back to raw text under the pointer.
      ev.preventDefault()
      ev.stopPropagation()
      onClick()
    })
  }
  return el
}

function build(view: EditorView, hooks: EditorHooks): DecorationSet {
  const builder = new RangeSetBuilder<Decoration>()
  if (!hooks.enabled() || !hooks.isGrimoireFile(view)) return builder.finish()
  const doc = view.state.doc
  // Memory notes are small, and a dispute links two lines anywhere in the
  // note, so the whole note is scanned rather than just the viewport.
  const scanned = scanNote(doc.toString())
  if (scanned.length === 0) return builder.finish()
  const cursorLines = new Set(
    view.state.selection.ranges.map((r) => doc.lineAt(r.head).number - 1),
  )
  for (const s of scanned) {
    const line = doc.line(s.line + 1)
    builder.add(line.from, line.from, Decoration.line({
      class: `grimoire-mem grimoire-mem-${s.badge.kind}`,
    }))
    const widget = new BadgeWidget(s.badge, s, hooks)
    const raw = cursorLines.has(s.line) || hooks.showTrailers()
    if (s.entry.trailerFrom >= 0 && !raw) {
      builder.add(line.from + s.entry.trailerFrom, line.from + s.entry.trailerTo,
        Decoration.replace({ widget }))
    } else {
      builder.add(line.to, line.to, Decoration.widget({ widget, side: 1 }))
    }
  }
  return builder.finish()
}

export function memoryBadges(hooks: EditorHooks) {
  return ViewPlugin.fromClass(
    class {
      decorations: DecorationSet
      constructor(view: EditorView) {
        this.decorations = build(view, hooks)
      }
      update(u: ViewUpdate) {
        if (u.docChanged || u.selectionSet || u.viewportChanged || u.transactions.some((t) => t.reconfigured)) {
          this.decorations = build(u.view, hooks)
        }
      }
    },
    { decorations: (v) => v.decorations },
  )
}
