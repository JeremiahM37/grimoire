// The memory-bullet grammar, ported from go/internal/memory/entry.go.
//
// Grimoire stores each remembered fact as one markdown bullet:
//
//   - **2026-08-14 09:00 · codex · deploy** — The API listens on 8080 <!--m id=… -->
//
// The plugin has to read those lines exactly the way the server does, because
// the thing it shows — who wrote this line, and does the server treat it as
// yours — is decided by that parse. A disagreement between the two would put a
// "yours" badge on a line the server will let an agent overwrite. The golden
// file in test/fixtures is produced by the Go parser and checked here, so the
// two cannot drift silently.

import { sha256Hex } from './sha256'

export type Authority = 'human' | 'agent' | 'pulled'

export interface MemoryLine {
  id: string
  text: string
  stamp: string
  agent: string
  task: string
  session: string
  category: string
  expires: string
  immutable: boolean
  supersededBy: string
  supersededAt: string
  challenges: string
  origin: string
  /** Declared by `by=human` in the trailer. */
  human: boolean
  /** Inferred: no trailer, or the id no longer hashes to the line's content. */
  handWritten: boolean
  struck: boolean
  /** Character offsets of the `<!--m … -->` trailer within the line, if any. */
  trailerFrom: number
  trailerTo: number
}

// Go's RE2 `\s` is ASCII-only, so these spell out the same class rather than
// using JavaScript's Unicode-wide `\s`.
const S = '[\\t\\n\\f\\r ]'
const bulletRE = new RegExp(`^-${S}+(~~)?\\*\\*(.+?)\\*\\*${S}+—${S}+(.*)$`)
const trailerRE = new RegExp(`${S}*<!--m${S}+([^>]*?)${S}*-->${S}*$`)
const strikeRE = new RegExp(`~~${S}*$`)
const punctRE = new RegExp(`[^\\p{L}\\p{N}\\t\\n\\f\\r ]+`, 'gu')
const spaceRE = new RegExp(`${S}+`, 'g')
const mintedRE = /^[0-9a-f]{12}(-[0-9]+)?$/

/** Origins that mean "the operator" (go/internal/trust SelfOrigins). */
const selfOrigins = new Set(['', 'self', 'user', 'me', 'local'])

/** Lowercase, punctuation to spaces, single-spaced: memory.Normalize. */
export function normalize(s: string): string {
  s = s.trim().toLowerCase()
  s = s.replace(punctRE, ' ')
  return s.replace(spaceRE, ' ').trim()
}

/** The content hash a fact's id is minted from: memory.DeriveID. */
export function deriveID(stamp: string, agent: string, text: string): string {
  return sha256Hex(stamp + '\x00' + agent + '\x00' + normalize(text)).slice(0, 12)
}

function unescapeField(v: string): string {
  // strings.NewReplacer scans left to right and takes the first match at each
  // position, which a single alternation regex reproduces.
  return v.replace(/\\s|\\\\|\\g/g, (m) => (m === '\\s' ? ' ' : m === '\\g' ? '>' : '\\'))
}

function trimRightSpaceTab(s: string): string {
  return s.replace(/[ \t]+$/, '')
}

/** Parse one line; null when it is not a memory bullet. */
export function parseMemoryLine(line: string): MemoryLine | null {
  const trimmed = trimRightSpaceTab(line)
  const m = bulletRE.exec(trimmed)
  if (!m) return null
  const struck = m[1] === '~~'
  const attribution = m[2]
  let rest = m[3]
  const e: MemoryLine = {
    id: '', text: '', stamp: '', agent: '', task: '', session: '', category: '',
    expires: '', immutable: false, supersededBy: '', supersededAt: '', challenges: '',
    origin: '', human: false, handWritten: false, struck,
    trailerFrom: -1, trailerTo: -1,
  }
  const t = trailerRE.exec(rest)
  if (t) {
    for (const field of t[1].split(/[\t\n\f\r ]+/)) {
      const eq = field.indexOf('=')
      if (eq < 0) continue
      const k = field.slice(0, eq)
      const v = unescapeField(field.slice(eq + 1))
      switch (k) {
        case 'id': e.id = v; break
        case 'session': e.session = v; break
        case 'cat': e.category = v; break
        case 'exp': e.expires = v; break
        case 'sup': e.supersededBy = v; break
        case 'supat': e.supersededAt = v; break
        case 'immutable': e.immutable = v === '1' || v === 'true'; break
        case 'org': e.origin = v; break
        case 'by': e.human = v === 'human'; break
        case 'chal': e.challenges = v; break
      }
    }
    rest = rest.slice(0, rest.length - t[0].length)
    // The trailer's position in the original line, for decorations. It sits
    // at the end of the trimmed line; trailing spaces after it are not part
    // of it.
    e.trailerTo = trimmed.length
    e.trailerFrom = trimmed.length - t[0].length
  }
  if (struck) rest = trimRightSpaceTab(rest).replace(strikeRE, '')
  e.text = rest.trim()
  const parts = attribution.split(' · ')
  e.stamp = (parts[0] ?? '').trim()
  if (parts.length > 1) e.agent = parts[1].trim()
  if (parts.length > 2) e.task = parts.slice(2).join(' · ').trim()

  if (e.id === '') {
    e.id = deriveID(e.stamp, e.agent, e.text)
    e.handWritten = true
  } else if (mintedRE.test(e.id) && !idMatchesContent(e)) {
    e.handWritten = true
  }
  return e
}

function idMatchesContent(e: MemoryLine): boolean {
  let base = e.id
  const i = base.lastIndexOf('-')
  if (i > 0 && /^[0-9]+$/.test(base.slice(i + 1))) base = base.slice(0, i)
  return base === deriveID(e.stamp, e.agent, e.text)
}

export function untrusted(e: MemoryLine): boolean {
  return !selfOrigins.has(e.origin.trim().toLowerCase())
}

/** memory.Entry.Authority, with the lattice on (the server default). */
export function authorityOf(e: MemoryLine): Authority {
  if (e.human || e.handWritten) return 'human'
  if (untrusted(e)) return 'pulled'
  return 'agent'
}

export function expired(e: MemoryLine, now: Date = new Date()): boolean {
  if (!e.expires) return false
  const t = Date.parse(e.expires)
  return !Number.isNaN(t) && now.getTime() > t
}

/**
 * Declare a line as yours: add `by=human` to its trailer.
 *
 * Only the trailer changes, so the id still hashes to the content and the
 * server reads a declared — not inferred — human fact, which an agent's later
 * write may challenge but not overwrite. A line with no trailer is already
 * inferred to be yours and is returned unchanged.
 */
export function vouch(line: string): string {
  const e = parseMemoryLine(line)
  if (!e || e.trailerFrom < 0 || e.human) return line
  const trimmed = trimRightSpaceTab(line)
  const trailer = trimmed.slice(e.trailerFrom)
  const close = trailer.lastIndexOf('-->')
  const inner = trailer.slice(0, close).replace(/[ \t]+$/, '')
  return trimmed.slice(0, e.trailerFrom) + inner + ' by=human-->' + line.slice(trimmed.length)
}
