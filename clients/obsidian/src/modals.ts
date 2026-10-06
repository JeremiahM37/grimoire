import { App, Modal, Notice, Setting } from 'obsidian'
import type { RetrievedChunk } from './api'
import type GrimoirePlugin from './main'

/**
 * "What would my agent see?" — the exact chunks retrieval hands an agent for a
 * question. Asking it before blaming the agent separates "it never saw the
 * note" from "it saw the note and ignored it".
 */
export class RetrieveModal extends Modal {
  private results!: HTMLElement
  private seq = 0

  constructor(app: App, private plugin: GrimoirePlugin, private initial: string) {
    super(app)
  }

  onOpen(): void {
    this.titleEl.setText('What would my agent see?')
    this.modalEl.addClass('grimoire-modal')
    const input = this.contentEl.createEl('input', {
      cls: 'grimoire-query',
      attr: { type: 'text', placeholder: 'Ask what your agent would ask…' },
    })
    input.value = this.initial
    this.results = this.contentEl.createDiv({ cls: 'grimoire-results' })
    input.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') {
        ev.preventDefault()
        void this.run(input.value)
      }
    })
    input.focus()
    if (this.initial.trim()) void this.run(this.initial)
  }

  private async run(q: string): Promise<void> {
    q = q.trim()
    if (!q) return
    const mine = ++this.seq
    this.results.empty()
    this.results.createDiv({ cls: 'grimoire-empty', text: 'Retrieving…' })
    let chunks: RetrievedChunk[]
    try {
      chunks = await this.plugin.client().retrieve(q, 8)
    } catch (e) {
      if (mine !== this.seq) return
      this.results.empty()
      this.results.createDiv({ cls: 'grimoire-empty', text: `Grimoire: ${(e as Error).message}` })
      return
    }
    if (mine !== this.seq) return
    this.results.empty()
    if (chunks.length === 0) {
      this.results.createDiv({
        cls: 'grimoire-empty',
        text: 'Nothing. An agent asking this gets no context from your notes.',
      })
      return
    }
    this.results.createDiv({
      cls: 'grimoire-card-meta',
      text: `${chunks.length} chunk${chunks.length === 1 ? '' : 's'}, in the order the agent receives them`,
    })
    for (const c of chunks) {
      const card = this.results.createDiv({ cls: 'grimoire-card grimoire-chunk' })
      const head = card.createDiv({ cls: 'grimoire-card-meta' })
      const link = head.createEl('a', { text: c.path, href: '#' })
      link.onclick = (ev) => {
        ev.preventDefault()
        this.close()
        void this.plugin.openEntry(c.path, '')
      }
      if (typeof c.score === 'number') head.createSpan({ text: ` · score ${c.score.toFixed(3)}` })
      if (c.trust === 'untrusted') head.createSpan({ cls: 'grimoire-warn', text: ' · fenced as untrusted' })
      card.createEl('pre', { cls: 'grimoire-chunk-text', text: c.chunk })
    }
  }

  onClose(): void {
    this.contentEl.empty()
  }
}

/**
 * "Tell my agents" — record a fact as a person. It goes on the top rung of the
 * authority lattice, so an agent's later write can dispute it but not replace it.
 */
export class TellModal extends Modal {
  constructor(app: App, private plugin: GrimoirePlugin, private text: string, private topic: string) {
    super(app)
  }

  onOpen(): void {
    this.titleEl.setText('Tell my agents')
    this.modalEl.addClass('grimoire-modal')
    this.contentEl.createEl('p', {
      cls: 'grimoire-card-meta',
      text: 'Saved as your fact. Agents can dispute it, but cannot overwrite it.',
    })
    const area = this.contentEl.createEl('textarea', { cls: 'grimoire-tell', attr: { rows: '3' } })
    area.value = this.text
    new Setting(this.contentEl)
      .setName('Topic')
      .setDesc('The memory note it goes in: memory/<topic>.md')
      .addText((t) => t.setValue(this.topic).onChange((v) => (this.topic = v)))
    new Setting(this.contentEl).addButton((b) =>
      b.setButtonText('Save').setCta().onClick(async () => {
        const text = area.value.trim()
        const topic = this.topic.trim() || 'notes'
        if (!text) return
        b.setDisabled(true)
        try {
          await this.plugin.client().rememberAsHuman(text, topic, this.plugin.settings.you)
          new Notice(`Saved to memory/${topic}.md as yours`)
          this.close()
          void this.plugin.refresh()
        } catch (e) {
          new Notice(`Grimoire: ${(e as Error).message}`)
          b.setDisabled(false)
        }
      }),
    )
    area.focus()
  }

  onClose(): void {
    this.contentEl.empty()
  }
}
