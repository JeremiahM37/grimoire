// The slice of Grimoire's HTTP API the plugin uses.
//
// Transport is injected: inside Obsidian it is `requestUrl` (which is not
// subject to CORS, so the server needs no change to be reachable from the app),
// and in the tests it is plain fetch against a real server.

export interface HttpRequest {
  url: string
  method: string
  headers: Record<string, string>
  body?: string
}

export interface HttpResponse {
  status: number
  text: string
}

export type Transport = (req: HttpRequest) => Promise<HttpResponse>

export class GrimoireError extends Error {
  constructor(public status: number, message: string) {
    super(message)
    this.name = 'GrimoireError'
  }
}

export interface Challenge {
  /** The agent's contested claim. */
  id: string
  text: string
  agent?: string
  stamp?: string
  note: string
  /** The fact it disagrees with and was not allowed to overwrite. */
  contested_id: string
  contested_text: string
  contested_authority: string
  contested_stamp?: string
}

export interface Change {
  kind: 'learned' | 'changed' | 'retracted' | 'expired' | string
  at: string
  id: string
  text: string
  path: string
  agent?: string
  topic?: string
  trust?: string
  was?: string
}

export interface ChangesOut {
  changes: Change[]
  counts: Record<string, number>
  since: string
}

export interface AgentActivity {
  agent: string
  facts: number
  challenges: number
  last_seen?: string
  first_seen?: string
}

export interface RetrievedChunk {
  path: string
  title: string
  chunk: string
  score?: number
  trust?: string
}

export interface RememberResult {
  event?: string
  id?: string
  path?: string
  [k: string]: unknown
}

export class GrimoireClient {
  constructor(
    private base: string,
    private token: string,
    private transport: Transport,
  ) {
    this.base = base.replace(/\/+$/, '')
  }

  private async call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (this.token) headers.Authorization = `Bearer ${this.token}`
    // Reads the plugin makes on a person's behalf are attributed to the app,
    // so they are not mistaken for an agent's in the server's own activity.
    headers['X-Grimoire-Agent'] = 'obsidian'
    let payload: string | undefined
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json'
      payload = JSON.stringify(body)
    }
    const res = await this.transport({ url: this.base + path, method, headers, body: payload })
    if (res.status < 200 || res.status >= 300) {
      let msg = res.text
      try {
        const j = JSON.parse(res.text)
        msg = j.error ?? j.detail ?? res.text
      } catch {
        // not JSON; keep the raw text
      }
      throw new GrimoireError(res.status, `${res.status} ${String(msg).trim() || 'request failed'}`)
    }
    return (res.text ? JSON.parse(res.text) : undefined) as T
  }

  health(): Promise<{ status?: string; notes?: number; version?: string; [k: string]: unknown }> {
    return this.call('GET', '/api/health')
  }

  challenges(): Promise<Challenge[]> {
    return this.call('GET', '/api/memory/challenges')
  }

  /** Settle one disagreement: uphold keeps your fact, concede accepts the agent's. */
  resolveChallenge(note: string, id: string, resolution: 'uphold' | 'concede'): Promise<unknown> {
    return this.call('POST', '/api/memory/challenge', { note, id, resolution })
  }

  changes(since = '7d', limit = 100): Promise<ChangesOut> {
    const q = new URLSearchParams({ since, limit: String(limit) })
    return this.call('GET', `/api/memory/changes?${q}`)
  }

  async agents(): Promise<AgentActivity[]> {
    const out = await this.call<{ agents?: AgentActivity[] }>('GET', '/api/usage/agents')
    return out.agents ?? []
  }

  /** Exactly the chunks an agent would be handed for this question. */
  retrieve(q: string, k = 8): Promise<RetrievedChunk[]> {
    const qs = new URLSearchParams({ q, k: String(k) })
    return this.call('GET', `/api/retrieve?${qs}`)
  }

  /**
   * Record a fact as a PERSON. It lands on the top rung of the authority
   * lattice: an agent's later write may challenge it but not overwrite it.
   */
  rememberAsHuman(text: string, topic: string, you: string): Promise<RememberResult> {
    return this.call('POST', '/api/memory', { text, topic, agent: you, human: true })
  }
}
