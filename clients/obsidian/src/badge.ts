// What the badge on a memory line says. Shared by the editor (Live Preview and
// source mode) and reading view, and kept free of Obsidian imports so it can be
// tested directly.

import { authorityOf, expired, type MemoryLine } from './memoryline'

export interface Badge {
  label: string
  /** CSS modifier: human | agent | pulled | dispute | replaced | expired */
  kind: string
  title: string
  /** Hue for an agent's chip, so each agent keeps one color. */
  hue: number
}

export function agentHue(agent: string): number {
  let h = 0
  for (const ch of agent) h = (h * 31 + ch.codePointAt(0)!) >>> 0
  return h % 360
}

/**
 * @param disputedBy the agent disputing this line, when another live line in
 *   the same note challenges it.
 */
export function badgeFor(e: MemoryLine, now: Date = new Date(), disputedBy = ''): Badge {
  const auth = authorityOf(e)
  const lines = [
    e.agent ? `Written by ${e.agent}` : 'No author recorded',
    e.stamp && `at ${e.stamp}`,
    e.task && `task: ${e.task}`,
    e.session && `session: ${e.session}`,
    e.category && `category: ${e.category}`,
    e.origin && `came from: ${e.origin}`,
    `id ${e.id}`,
  ].filter(Boolean)
  const hue = agentHue(e.agent || 'you')

  if (e.struck) {
    return {
      label: 'replaced', kind: 'replaced', hue,
      title: [`Superseded${e.supersededAt ? ` at ${e.supersededAt}` : ''} by ${e.supersededBy}`, ...lines].join('\n'),
    }
  }
  if (expired(e, now)) {
    return { label: 'expired', kind: 'expired', hue, title: [`Expired ${e.expires}`, ...lines].join('\n') }
  }
  if (e.challenges) {
    return {
      label: `⚑ ${e.agent || 'agent'} disputes yours`, kind: 'dispute', hue,
      title: ['Your agent disagrees with a fact you wrote, and was not allowed to overwrite it.',
        'Settle it in the Grimoire panel.', ...lines].join('\n'),
    }
  }
  if (auth === 'human') {
    // A hand-edited line still carries the trailer the agent wrote; a line
    // typed from scratch has none.
    const edited = e.handWritten && e.trailerFrom >= 0 && e.agent
    const label = e.human ? '✓ yours' : edited ? `✎ you edited ${e.agent}'s` : '✎ yours'
    if (disputedBy) {
      return {
        label: `${label} · ⚑ ${disputedBy} disagrees`, kind: 'dispute', hue,
        title: [`${disputedBy} believes something else and was not allowed to overwrite this.`,
          'Settle it in the Grimoire panel.', ...lines].join('\n'),
      }
    }
    return {
      label, kind: 'human', hue,
      title: ['Yours. An agent may dispute this line but cannot overwrite it.', ...lines].join('\n'),
    }
  }
  if (auth === 'pulled') {
    return {
      label: `⚠ ${e.agent || 'agent'} · from ${e.origin}`, kind: 'pulled', hue,
      title: ['Copied from a source other people can write. It cannot override your facts.', ...lines].join('\n'),
    }
  }
  return {
    label: e.agent || 'agent', kind: 'agent', hue,
    title: ['Written by an agent. Edit the line to correct it, and your version wins.', ...lines].join('\n'),
  }
}
