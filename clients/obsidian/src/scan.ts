// Reads a whole note's memory lines once, so each line's badge can know about
// the others: a line is disputed when another live line challenges its id.

import { parseMemoryLine, type MemoryLine } from './memoryline'
import { badgeFor, type Badge } from './badge'

export interface ScannedLine {
  /** 0-based line number. */
  line: number
  entry: MemoryLine
  badge: Badge
}

export function scanNote(text: string, now: Date = new Date()): ScannedLine[] {
  const parsed: Array<{ line: number; entry: MemoryLine }> = []
  text.split('\n').forEach((raw, line) => {
    const entry = parseMemoryLine(raw)
    if (entry) parsed.push({ line, entry })
  })
  const disputers = new Map<string, string>()
  for (const { entry } of parsed) {
    if (entry.challenges && !entry.struck) disputers.set(entry.challenges, entry.agent || 'an agent')
  }
  return parsed.map(({ line, entry }) => ({
    line,
    entry,
    badge: badgeFor(entry, now, entry.struck ? '' : disputers.get(entry.id) ?? ''),
  }))
}
