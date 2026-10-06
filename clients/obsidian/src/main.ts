import {
  App,
  type Editor,
  editorInfoField,
  MarkdownView,
  Notice,
  Plugin,
  PluginSettingTab,
  requestUrl,
  Setting,
  TFile,
} from 'obsidian'
import type { EditorView } from '@codemirror/view'
import { GrimoireClient, type Transport } from './api'
import { memoryBadges, renderBadge } from './editor'
import { parseMemoryLine, vouch } from './memoryline'
import { RetrieveModal, TellModal } from './modals'
import { memoryTopic, toGrimoirePath, toVaultPath } from './paths'
import { scanNote } from './scan'
import { GrimoireView, type PanelState, VIEW_TYPE } from './view'

interface Settings {
  serverUrl: string
  token: string
  you: string
  folder: string
  pollSeconds: number
  badges: boolean
  showTrailers: boolean
}

const DEFAULTS: Settings = {
  serverUrl: 'http://127.0.0.1:9111',
  token: '',
  you: 'me',
  folder: '',
  pollSeconds: 60,
  badges: true,
  showTrailers: false,
}

const transport: Transport = async (req) => {
  const res = await requestUrl({
    url: req.url,
    method: req.method,
    headers: req.headers,
    body: req.body,
    contentType: req.headers['Content-Type'],
    throw: false,
  })
  return { status: res.status, text: res.text }
}

export default class GrimoirePlugin extends Plugin {
  settings: Settings = { ...DEFAULTS }
  state: PanelState = { online: false, error: '', challenges: [], changes: [], agents: [], loadedAt: null }
  private status!: HTMLElement
  private seenChallenges: Set<string> | null = null
  private timer: number | null = null
  private refreshing: Promise<void> | null = null

  async onload(): Promise<void> {
    await this.loadSettings()

    this.registerView(VIEW_TYPE, (leaf) => new GrimoireView(leaf, this))
    this.addRibbonIcon('book-marked', 'Grimoire: agent memory', () => this.openPanel())

    this.status = this.addStatusBarItem()
    this.status.addClass('grimoire-status')
    this.status.onClickEvent(() => this.openPanel())
    this.renderStatus()

    this.registerEditorExtension(memoryBadges({
      enabled: () => this.settings.badges,
      showTrailers: () => this.settings.showTrailers,
      isGrimoireFile: (view: EditorView) => {
        const file = view.state.field(editorInfoField, false)?.file
        return !!file && this.grimoirePathOf(file.path) !== null
      },
      onBadgeClick: (badge) => {
        if (badge.kind === 'dispute') this.openPanel()
        else new Notice(badge.title, 8000)
      },
    }))

    this.registerMarkdownPostProcessor((el, ctx) => {
      if (!this.settings.badges || this.grimoirePathOf(ctx.sourcePath) === null) return
      const info = ctx.getSectionInfo(el)
      if (!info) return
      const scanned = scanNote(info.text)
      if (scanned.length === 0) return
      const byLine = new Map(scanned.map((s) => [s.line, s]))
      // A memory note is a flat list, so the section's bullet lines and its
      // top-level <li>s pair up in order.
      const lines = info.text.split('\n')
      const bulletLines: number[] = []
      for (let i = info.lineStart; i <= info.lineEnd; i++) {
        if (/^[-*+] /.test(lines[i] ?? '') || /^\d+[.)] /.test(lines[i] ?? '')) bulletLines.push(i)
      }
      const items = Array.from(el.querySelectorAll(':scope > ul > li, :scope > ol > li'))
      items.forEach((li, n) => {
        const s = byLine.get(bulletLines[n])
        if (!s) return
        li.addClass('grimoire-mem', `grimoire-mem-${s.badge.kind}`)
        li.appendChild(renderBadge(s.badge, () => {
          if (s.badge.kind === 'dispute') this.openPanel()
        }))
      })
    })

    this.addCommand({ id: 'open-panel', name: 'Open agent memory panel', callback: () => this.openPanel() })
    this.addCommand({ id: 'refresh', name: 'Refresh from server', callback: () => this.refresh() })
    this.addCommand({
      id: 'what-would-agent-see',
      name: 'What would my agent see?',
      editorCallback: (editor: Editor) => new RetrieveModal(this.app, this, editor.getSelection()).open(),
    })
    this.addCommand({
      id: 'what-would-agent-see-anywhere',
      name: 'What would my agent see? (blank)',
      callback: () => new RetrieveModal(this.app, this, '').open(),
    })
    this.addCommand({
      id: 'tell-agents',
      name: 'Tell my agents…',
      callback: () => {
        const editor = this.app.workspace.getActiveViewOfType(MarkdownView)?.editor
        const file = this.app.workspace.getActiveFile()
        const topic = (file && memoryTopic(file.path, this.settings.folder)) ?? 'notes'
        new TellModal(this.app, this, editor?.getSelection() ?? '', topic).open()
      },
    })
    this.addCommand({
      id: 'vouch-line',
      name: 'Make this memory line mine',
      editorCheckCallback: (checking: boolean, editor: Editor) => {
        const n = editor.getCursor().line
        const line = editor.getLine(n)
        const e = parseMemoryLine(line)
        if (!e) return false
        if (checking) return true
        if (e.human || e.handWritten) {
          new Notice('That line is already yours.')
          return true
        }
        const next = vouch(line)
        editor.replaceRange(next, { line: n, ch: 0 }, { line: n, ch: line.length })
        new Notice('Marked as yours. Agents can dispute it but not overwrite it.')
        return true
      },
    })

    this.addSettingTab(new GrimoireSettingTab(this.app, this))
    this.app.workspace.onLayoutReady(() => {
      void this.refresh()
      this.schedule()
    })
  }

  onunload(): void {
    if (this.timer !== null) window.clearInterval(this.timer)
  }

  async loadSettings(): Promise<void> {
    this.settings = { ...DEFAULTS, ...(await this.loadData()) }
  }

  async saveSettings(): Promise<void> {
    await this.saveData(this.settings)
    this.schedule()
    // Reconfiguring re-runs the editor extension with the new settings.
    this.app.workspace.updateOptions()
  }

  client(): GrimoireClient {
    return new GrimoireClient(this.settings.serverUrl, this.settings.token, transport)
  }

  grimoirePathOf(vaultPath: string): string | null {
    return toGrimoirePath(vaultPath, this.settings.folder)
  }

  private schedule(): void {
    if (this.timer !== null) window.clearInterval(this.timer)
    const secs = Math.max(10, this.settings.pollSeconds || 60)
    this.timer = window.setInterval(() => void this.refresh(), secs * 1000)
  }

  refresh(): Promise<void> {
    // Overlapping refreshes (a poll landing during a click) share one request.
    if (!this.refreshing) {
      this.refreshing = this.fetchState().finally(() => (this.refreshing = null))
    }
    return this.refreshing
  }

  private async fetchState(): Promise<void> {
    const c = this.client()
    try {
      const [challenges, changes, agents] = await Promise.all([c.challenges(), c.changes('7d', 100), c.agents()])
      this.state = { online: true, error: '', challenges, changes: changes.changes ?? [], agents, loadedAt: new Date() }
      this.announce(challenges.map((x) => x.id))
    } catch (e) {
      this.state = { ...this.state, online: false, error: (e as Error).message, loadedAt: new Date() }
    }
    this.renderStatus()
    for (const leaf of this.app.workspace.getLeavesOfType(VIEW_TYPE)) {
      if (leaf.view instanceof GrimoireView) leaf.view.render()
    }
  }

  /** One notice per new disagreement, never a repeat for one already shown. */
  private announce(ids: string[]): void {
    if (this.seenChallenges === null) {
      this.seenChallenges = new Set(ids)
      if (ids.length > 0) {
        this.notice(`${ids.length} disagreement${ids.length === 1 ? '' : 's'} with your agents to settle`)
      }
      return
    }
    const fresh = this.state.challenges.filter((c) => !this.seenChallenges!.has(c.id))
    for (const c of fresh) {
      this.seenChallenges.add(c.id)
      this.notice(`${c.agent || 'An agent'} disagrees with something you wrote: "${c.text}"`)
    }
  }

  private notice(text: string): void {
    const n = new Notice(text, 10000)
    n.noticeEl.addClass('grimoire-notice')
    n.noticeEl.onclick = () => this.openPanel()
  }

  private renderStatus(): void {
    const s = this.state
    this.status.empty()
    this.status.removeClass('is-dispute', 'is-offline')
    if (!s.loadedAt) {
      this.status.setText('Grimoire')
    } else if (!s.online) {
      this.status.addClass('is-offline')
      this.status.setText('Grimoire offline')
      this.status.setAttr('aria-label', s.error)
    } else if (s.challenges.length > 0) {
      this.status.addClass('is-dispute')
      this.status.setText(`⚑ ${s.challenges.length} to settle`)
      this.status.setAttr('aria-label', 'Your agents disagree with facts you wrote')
    } else {
      this.status.setText('Grimoire ✓')
      this.status.setAttr('aria-label', 'Connected; nothing to settle')
    }
  }

  async openPanel(): Promise<void> {
    const existing = this.app.workspace.getLeavesOfType(VIEW_TYPE)[0]
    const leaf = existing ?? this.app.workspace.getRightLeaf(false)
    if (!leaf) return
    if (!existing) await leaf.setViewState({ type: VIEW_TYPE, active: true })
    this.app.workspace.revealLeaf(leaf)
  }

  /** Open a Grimoire note and put the cursor on the bullet with this id. */
  async openEntry(grimoirePath: string, id: string): Promise<void> {
    const path = toVaultPath(grimoirePath, this.settings.folder)
    const file = this.app.vault.getAbstractFileByPath(path)
    if (!(file instanceof TFile)) {
      new Notice(`${path} is not in this vault. Check the Grimoire folder setting.`)
      return
    }
    const leaf = this.app.workspace.getLeaf(false)
    await leaf.openFile(file)
    if (!id) return
    const view = leaf.view instanceof MarkdownView ? leaf.view : null
    if (!view) return
    const editor = view.editor
    for (let i = 0; i < editor.lineCount(); i++) {
      if (parseMemoryLine(editor.getLine(i))?.id === id) {
        editor.setCursor({ line: i, ch: 0 })
        editor.scrollIntoView({ from: { line: i, ch: 0 }, to: { line: i, ch: 0 } }, true)
        return
      }
    }
  }
}

class GrimoireSettingTab extends PluginSettingTab {
  constructor(app: App, private plugin: GrimoirePlugin) {
    super(app, plugin)
  }

  display(): void {
    const { containerEl } = this
    const s = this.plugin.settings
    containerEl.empty()

    new Setting(containerEl)
      .setName('Server URL')
      .setDesc('Where Grimoire is running.')
      .addText((t) => t.setPlaceholder(DEFAULTS.serverUrl).setValue(s.serverUrl).onChange(async (v) => {
        s.serverUrl = v.trim() || DEFAULTS.serverUrl
        await this.plugin.saveSettings()
      }))
    new Setting(containerEl)
      .setName('Token')
      .setDesc('Bearer token, if your server requires one (GRIMOIRE_AUTH_TOKEN or an API key).')
      .addText((t) => {
        t.inputEl.type = 'password'
        t.setValue(s.token).onChange(async (v) => {
          s.token = v.trim()
          await this.plugin.saveSettings()
        })
      })
    new Setting(containerEl)
      .setName('Test connection')
      .addButton((b) => b.setButtonText('Test').onClick(async () => {
        try {
          const h = await this.plugin.client().health()
          new Notice(`Connected to Grimoire${h.version ? ` ${h.version}` : ''}`)
          await this.plugin.refresh()
        } catch (e) {
          new Notice(`Can't reach Grimoire: ${(e as Error).message}`)
        }
      }))
    new Setting(containerEl)
      .setName('Your name')
      .setDesc('Shown as the author of facts you tell your agents.')
      .addText((t) => t.setValue(s.you).onChange(async (v) => {
        s.you = v.trim() || DEFAULTS.you
        await this.plugin.saveSettings()
      }))
    new Setting(containerEl)
      .setName('Grimoire folder')
      .setDesc('Leave empty when Grimoire serves this whole vault. Otherwise, the folder inside this vault it serves.')
      .addText((t) => t.setValue(s.folder).onChange(async (v) => {
        s.folder = v.trim()
        await this.plugin.saveSettings()
      }))
    new Setting(containerEl)
      .setName('Badges on memory lines')
      .setDesc('Show who wrote each memory line, and whether it is yours.')
      .addToggle((t) => t.setValue(s.badges).onChange(async (v) => {
        s.badges = v
        await this.plugin.saveSettings()
      }))
    new Setting(containerEl)
      .setName('Show raw trailers')
      .setDesc('Keep the <!--m … --> metadata visible instead of folding it into the badge.')
      .addToggle((t) => t.setValue(s.showTrailers).onChange(async (v) => {
        s.showTrailers = v
        await this.plugin.saveSettings()
      }))
    new Setting(containerEl)
      .setName('Check every (seconds)')
      .setDesc('How often to look for new disagreements.')
      .addText((t) => t.setValue(String(s.pollSeconds)).onChange(async (v) => {
        const n = parseInt(v, 10)
        if (Number.isFinite(n) && n >= 10) {
          s.pollSeconds = n
          await this.plugin.saveSettings()
        }
      }))
  }
}
