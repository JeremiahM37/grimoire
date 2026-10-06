import { ItemView, Notice, setIcon, type WorkspaceLeaf } from 'obsidian'
import type { AgentActivity, Challenge, Change } from './api'
import { agentHue } from './badge'
import type GrimoirePlugin from './main'

export const VIEW_TYPE = 'grimoire-memory'

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`

export interface PanelState {
  online: boolean
  error: string
  challenges: Challenge[]
  changes: Change[]
  agents: AgentActivity[]
  loadedAt: Date | null
}

export class GrimoireView extends ItemView {
  private thisNoteOnly = false

  constructor(leaf: WorkspaceLeaf, private plugin: GrimoirePlugin) {
    super(leaf)
  }

  getViewType(): string {
    return VIEW_TYPE
  }

  getDisplayText(): string {
    return 'Grimoire'
  }

  getIcon(): string {
    return 'book-marked'
  }

  async onOpen(): Promise<void> {
    this.registerEvent(this.app.workspace.on('active-leaf-change', () => {
      if (this.thisNoteOnly) this.render()
    }))
    this.render()
    await this.plugin.refresh()
  }

  render(): void {
    const s = this.plugin.state
    const root = this.contentEl
    root.empty()
    root.addClass('grimoire-panel')

    const head = root.createDiv({ cls: 'grimoire-panel-head' })
    head.createSpan({
      cls: `grimoire-dot ${s.online ? 'is-online' : 'is-offline'}`,
      attr: { 'aria-label': s.online ? 'Connected' : s.error || 'Not connected' },
    })
    head.createSpan({ cls: 'grimoire-panel-title', text: 'Agent memory' })
    const refresh = head.createEl('button', { cls: 'clickable-icon', attr: { 'aria-label': 'Refresh' } })
    setIcon(refresh, 'refresh-cw')
    refresh.onclick = () => this.plugin.refresh()

    if (!s.online) {
      const box = root.createDiv({ cls: 'grimoire-empty' })
      box.createEl('p', { text: s.loadedAt ? `Can't reach Grimoire: ${s.error}` : 'Connecting…' })
      if (s.loadedAt) box.createEl('p', { text: 'Check the server URL and token in settings.' })
      return
    }

    this.renderChallenges(root, s.challenges)
    this.renderActivity(root, s.changes)
    this.renderAgents(root, s.agents)
  }

  private section(root: HTMLElement, title: string, count?: number): HTMLElement {
    const sec = root.createDiv({ cls: 'grimoire-section' })
    const h = sec.createDiv({ cls: 'grimoire-section-title' })
    h.createSpan({ text: title })
    if (count !== undefined) h.createSpan({ cls: 'grimoire-count', text: String(count) })
    return sec
  }

  private agentChip(parent: HTMLElement, agent: string): void {
    const chip = parent.createSpan({ cls: 'grimoire-badge grimoire-badge-agent', text: agent || 'agent' })
    chip.style.setProperty('--grimoire-hue', String(agentHue(agent || 'agent')))
  }

  private renderChallenges(root: HTMLElement, list: Challenge[]): void {
    const sec = this.section(root, 'Disagreements to settle', list.length)
    if (list.length === 0) {
      sec.createDiv({ cls: 'grimoire-empty', text: 'None. Your agents agree with everything you wrote.' })
      return
    }
    for (const c of list) {
      const card = sec.createDiv({ cls: 'grimoire-card grimoire-challenge' })
      const who = card.createDiv({ cls: 'grimoire-card-meta' })
      this.agentChip(who, c.agent ?? '')
      who.createSpan({ text: ` thinks${c.stamp ? ` (${c.stamp})` : ''}` })
      card.createDiv({ cls: 'grimoire-claim grimoire-claim-agent', text: c.text })
      card.createDiv({ cls: 'grimoire-card-meta', text: `but ${c.contested_authority === 'human' ? 'you' : c.contested_authority} wrote` })
      card.createDiv({ cls: 'grimoire-claim grimoire-claim-human', text: c.contested_text })

      const actions = card.createDiv({ cls: 'grimoire-actions' })
      const keep = actions.createEl('button', { cls: 'mod-cta', text: 'Keep mine' })
      const concede = actions.createEl('button', { text: `${c.agent || 'Agent'} is right` })
      const open = actions.createEl('button', { cls: 'clickable-icon', attr: { 'aria-label': `Open ${c.note}` } })
      setIcon(open, 'file-text')
      open.onclick = () => this.plugin.openEntry(c.note, c.contested_id)
      const settle = async (resolution: 'uphold' | 'concede') => {
        keep.disabled = concede.disabled = true
        try {
          await this.plugin.client().resolveChallenge(c.note, c.id, resolution)
          new Notice(resolution === 'uphold' ? 'Kept yours. The agent\'s claim is struck through.' :
            `Accepted ${c.agent || 'the agent'}'s version.`)
        } catch (e) {
          new Notice(`Grimoire: ${(e as Error).message}`)
        }
        await this.plugin.refresh()
      }
      keep.onclick = () => settle('uphold')
      concede.onclick = () => settle('concede')
    }
  }

  private renderActivity(root: HTMLElement, all: Change[]): void {
    const active = this.app.workspace.getActiveFile()
    const notePath = active ? this.plugin.grimoirePathOf(active.path) : null
    const list = this.thisNoteOnly ? all.filter((c) => c.path === notePath) : all
    const sec = this.section(root, 'What agents learned (7 days)', list.length)
    const toggle = sec.createEl('label', { cls: 'grimoire-toggle' })
    const box = toggle.createEl('input', { attr: { type: 'checkbox' } })
    box.checked = this.thisNoteOnly
    toggle.appendText(' This note only')
    box.onchange = () => {
      this.thisNoteOnly = box.checked
      this.render()
    }
    if (list.length === 0) {
      sec.createDiv({ cls: 'grimoire-empty', text: this.thisNoteOnly ? 'Nothing in this note.' : 'Nothing recently.' })
      return
    }
    for (const c of list.slice(0, 50)) {
      const row = sec.createDiv({ cls: `grimoire-change grimoire-change-${c.kind}` })
      const meta = row.createDiv({ cls: 'grimoire-card-meta' })
      this.agentChip(meta, c.agent ?? '')
      meta.createSpan({ text: ` ${c.kind} · ${c.at}` })
      if (c.trust === 'untrusted') meta.createSpan({ cls: 'grimoire-warn', text: ' · from an outside source' })
      const text = row.createDiv({ cls: 'grimoire-change-text', text: c.text })
      text.onclick = () => this.plugin.openEntry(c.path, c.id)
      row.createDiv({ cls: 'grimoire-path', text: c.path })
    }
  }

  private renderAgents(root: HTMLElement, agents: AgentActivity[]): void {
    const known = agents.filter((a) => a.agent && !a.agent.startsWith('('))
    const sec = this.section(root, 'Agents', known.length)
    for (const a of known) {
      const row = sec.createDiv({ cls: 'grimoire-agent-row' })
      this.agentChip(row, a.agent)
      row.createSpan({
        cls: 'grimoire-card-meta',
        text: ` ${plural(a.facts, 'fact')}${a.challenges ? ` · ${plural(a.challenges, 'dispute')}` : ''}${a.last_seen ? ` · last ${a.last_seen}` : ''}`,
      })
    }
  }
}
