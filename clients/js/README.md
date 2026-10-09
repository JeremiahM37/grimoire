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

## Memory banks

`@jeremiahm37/grimoire/banks` adds memory banks — retain raw content, let the
server extract the facts, recall by meaning, words, entities and time — and a
memory wrapper for any OpenAI-compatible client. See
[docs/MEMORY_BANKS.md](../../docs/MEMORY_BANKS.md).

```js
import Grimoire from '@jeremiahm37/grimoire'
import { Bank, Banks, withMemory } from '@jeremiahm37/grimoire/banks'

const g = new Grimoire('http://localhost:9111', { token })
const bank = new Bank(g, 'support')
await bank.retain([{ speaker: 'Dana', text: 'Move the migration to May.' }], { documentId: 'chat-42' })
const { results } = await bank.recall('when is the migration?', { maxTokens: 2048 })

const llm = withMemory(new OpenAI(), bank, { sessionId: 'chat-7', background: true })
await llm.chat.completions.create({ model, messages })            // recalls, injects, retains
await llm.chat.completions.create({ model, messages, grimoire: { inject: false } })
```

Reasoning over a bank:

```js
const { text, mode, based_on } = await bank.reflect('what does Dana think of the migration?')
const { operation_id } = await bank.retain(transcript, { documentId: 's-1', async: true })
await bank.waitOperation(operation_id)                  // completed | failed | cancelled
await bank.consolidate()                                // facts -> observations
const { items } = await bank.observations()
const { mental_model_id } = await bank.createMentalModel('Dana', 'Who is Dana?', { folder: 'people' })
await bank.refreshMentalModel(mental_model_id)          // 'people/dana'
await bank.updateMentalModel('people/dana', { body: 'My own words.' })  // a person's edit wins
await bank.acceptProposal('people/dana')                // or rejectProposal
await bank.createDirective('Answer in one sentence.', { name: 'Brief' })
await bank.importTemplate('coding-agent')
```

Consolidation and mental-model refreshes need a language model on the server;
without one they reject with `ModelRequired` (`error.code === 'model_required'`),
while reflect answers extractively. Moving a mental model to another folder
changes its id. On a server from before these routes they reject with
`NotAvailable` (a `NotFound`).

## Scoping and lifetime

```js
await g.add('the staging box is smaller', { session: 'run-42', category: 'infra' })
await g.add('priya is on call', { expires_in: '72h' })
await g.add('never touch prod', { immutable: true })
await g.add('grimoire is build 1.4.0', { fresh: 'volatile', check: 'grimoire version' })

await g.search('', { session: 'run-42' })
await g.scopes()
```

## Coming from mem0

`add`, `search`, `getAll` and `delete` line up. The differences: scope is
`session` / `agent` rather than `run_id` / `agent_id`, `delete` takes the note
path alongside the id (facts live in files you own), and `add` reconciles
rather than accumulating.

## Vercel AI SDK

Memory reaches a model through tools, so the adapter is a tool set:

```js
import { generateText, jsonSchema } from 'ai'
import Grimoire from '@jeremiahm37/grimoire'
import { grimoireTools } from '@jeremiahm37/grimoire/tools'

const client = new Grimoire('http://localhost:9111', { token: '...' })
const tools = grimoireTools(client, { jsonSchema })

await generateText({ model, tools, prompt: 'what indentation do I prefer?' })
```

Six tools: `recallMemory`, `rememberFact`, `forgetFact`, `memoryGraph`,
`rateMemory`, `askNotes`. Pass `include: [...]` for a subset.

The descriptions say what changes a model's behaviour, not just what each tool
can do — that writes reconcile, that recall returns current beliefs only, and
when to retract rather than overwrite. A tool list that only says what is
callable produces an agent that hedges every correction into a new fact
competing with the old one.

`jsonSchema` is the AI SDK's own helper, passed in so this package keeps no
dependencies. Leave it out and `inputSchema` stays a plain JSON Schema object,
which most other frameworks accept as-is.

## Knowing when the notes don't say

`ask` returns a `supported` verdict alongside the answer:

```
await g.ask('what port does the deploy use?')
# {"answer": "...", "supported": "ungrounded", "citations": [...]}
```

`grounded` means the notes state what you asked for, `ungrounded` means they
are about the right topic but do not contain the answer, `unknown` means no
reader judged it. Acting on `ungrounded` is the point — retrieved passages
being on-topic is not evidence that they answer the question, and the
similarity scores cannot tell you the difference (measured: a top-cosine
threshold scores AUC 0.55 on that task, and inverts on multi-hop questions). The verdict
costs no extra model call; it rides in the same completion as the answer.

## Beyond memory

`ask`, `searchNotes`, `readNote`, `writeNote`, `getFact`, `briefing`, `health`.
Errors are typed: `NotFound`, `Unauthorized`, `VaultLocked`, and `GrimoireError`.

## Development

```bash
npm test    # node --test test/
```
