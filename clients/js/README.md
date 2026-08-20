# @jeremiahm37/grimoire

JavaScript and TypeScript client for
[Grimoire](https://github.com/JeremiahM37/grimoire) — self-hosted memory,
knowledge and credentials for AI agents.

No dependencies and no build step: the published artifact is the source, standard
ESM against the platform's `fetch`, with hand-written TypeScript declarations.
Works in Node ≥18, Deno, Bun and the browser.

```bash
npm install @jeremiahm37/grimoire
```

```js
import Grimoire, { stored } from '@jeremiahm37/grimoire'

const g = new Grimoire('http://localhost:9111', { token: '...', agent: 'my-agent' })

await g.add('the user prefers tabs', { topic: 'prefs', session: 'run-1' })
const result = await g.add('the user prefers spaces')
result.op          // 'UPDATE' — the old belief was superseded
stored(result)     // true

const facts = await g.search('indentation')
facts.map((f) => f.text)   // ['the user prefers spaces'] — only what is believed now
```

## Writes reconcile

| the new fact | what happens | `result.op` |
|---|---|---|
| says something new | stored | `ADD` |
| contradicts one on file | the old one is superseded | `UPDATE` |
| retracts one on file | the old one is struck through, nothing stored | `DELETE` |
| is already recorded | nothing is written | `NOOP` |

A superseded fact is **struck through, not deleted** — it stays in the markdown
note, which is what makes the history answerable:

```js
await g.search('indentation', { includeSuperseded: true })
await g.history('2026-08-14T09:00:00Z')   // what was believed then
```

## Scoping and lifetime

```js
await g.add('the staging box is smaller', { session: 'run-42', category: 'infra' })
await g.add('priya is on call', { expires_in: '72h' })
await g.add('never touch prod', { immutable: true })

await g.search('', { session: 'run-42' })
await g.scopes()
```

## Coming from mem0

`add`, `search`, `getAll` and `delete` line up. The differences: scope is
`session` / `agent` rather than `run_id` / `agent_id`, `delete` takes the note
path alongside the id (facts live in files you own), and `add` reconciles
rather than accumulating.

## Beyond memory

`ask`, `searchNotes`, `readNote`, `writeNote`, `getFact`, `briefing`, `health`.
Errors are typed: `NotFound`, `Unauthorized`, `VaultLocked`, and `GrimoireError`.

## Development

```bash
npm test    # node --test test/
```
