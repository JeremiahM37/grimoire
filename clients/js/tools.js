/**
 * Grimoire as tools for the Vercel AI SDK (and anything with the same shape).
 *
 * The AI SDK has no memory interface to implement — memory reaches a model
 * through tools. So this exports tool definitions rather than a store: give it
 * a client and it returns `{ description, inputSchema, execute }` per tool,
 * which is exactly what `generateText({ tools })` wants.
 *
 * `inputSchema` is a plain JSON Schema object by default, so this file needs
 * no dependency and works with anything that accepts one. The AI SDK wants its
 * own `jsonSchema()` wrapper, so pass it in:
 *
 *     import { jsonSchema, generateText } from 'ai'
 *     import { grimoireTools } from '@jeremiahm37/grimoire/tools'
 *
 *     const tools = grimoireTools(client, { jsonSchema })
 *     await generateText({ model, tools, prompt })
 *
 * The descriptions carry what changes a model's behaviour, not just what each
 * tool can do: that writes reconcile, that recall returns current beliefs
 * only, and when to retract rather than overwrite. A tool list that only says
 * what is callable produces an agent that hedges every correction into a new
 * fact competing with the old one.
 */

const obj = (properties, required = []) => ({
  type: 'object',
  properties,
  required,
  additionalProperties: false,
})

const str = (description) => ({ type: 'string', description })
const int = (description) => ({ type: 'integer', description })
const bool = (description) => ({ type: 'boolean', description })

/**
 * Build the tool set.
 *
 * @param {import('./index.js').Grimoire} client
 * @param {{jsonSchema?: (schema: object) => unknown, include?: string[]}} [options]
 */
export function grimoireTools(client, options = {}) {
  // Identity by default: a raw JSON Schema is what most frameworks want, and
  // the AI SDK's wrapper is one import the caller already has.
  const wrap = options.jsonSchema ?? ((schema) => schema)

  const all = {
    recallMemory: {
      description:
        'Search what you and previous agents recorded about this user and their ' +
        'systems. Check it before re-deriving anything that smells like it was ' +
        'learned the hard way. Returns individual facts, currently-believed ones ' +
        'only — beliefs that were later replaced are left out unless asked for.',
      inputSchema: obj({
        query: str('what you are trying to remember'),
        limit: int('max facts (default 10)'),
        session: str('optional: only what was learned in this run'),
        category: str('optional: only this bucket'),
        includeSuperseded: bool('also return replaced beliefs, and what replaced them'),
      }),
      execute: ({ query, limit, session, category, includeSuperseded }) =>
        client.search(query ?? '', { limit: limit ?? 10, session, category, includeSuperseded }),
    },

    rememberFact: {
      description:
        'Persist something worth knowing later — a preference, a root cause, a ' +
        'convention you had to discover. Writes RECONCILE: if this contradicts ' +
        'something already recorded, the old belief is superseded rather than left ' +
        'to compete with it, and if it is already recorded nothing is written. The ' +
        'result says which happened (op: ADD / UPDATE / DELETE / NOOP), so report a ' +
        'correction plainly rather than hedging it into a new fact.',
      inputSchema: obj({
        text: str('what to remember, as a standalone statement'),
        topic: str('optional grouping, e.g. "preferences"'),
        session: str('optional run id, so this run\'s learnings recall together'),
        category: str('optional bucket, e.g. "preference", "gotcha"'),
        expires_in: str('optional time-to-live, e.g. "72h", for something true only for now'),
      }, ['text']),
      execute: (args) => client.add(args.text, args),
    },

    forgetFact: {
      description:
        'Retract one recorded fact by id (ids come from recallMemory). It stops ' +
        'being recalled but stays readable to the human, struck through. Use it when ' +
        'a fact is WRONG — not when it is merely out of date, which rememberFact ' +
        'already handles by superseding.',
      inputSchema: obj({
        id: str('the fact id, from recallMemory'),
        path: str('the note it lives in, from recallMemory'),
      }, ['id', 'path']),
      execute: ({ id, path }) => client.delete(path, id),
    },

    memoryGraph: {
      description:
        'What memory knows ABOUT a thing and what it is connected to — people, ' +
        'services, files, hosts. Use it when you have a name and no context. Edges ' +
        'name the facts that connect them, so read the evidence rather than trusting ' +
        'the connection. With no entity, returns what memory is mostly about.',
      inputSchema: obj({
        entity: str('the thing to start from'),
        depth: int('hops out from it (default 1, max 4)'),
      }),
      execute: ({ entity, depth }) => client.graph(entity ?? '', { depth: depth ?? 1 }),
    },

    rateMemory: {
      description:
        'Report whether a recalled fact earned its place. Call it when a fact turned ' +
        'out to be the one you needed, or turned out to be noise — not on every ' +
        'recall. It nudges ranking; it cannot bury a fact that is the only answer to ' +
        'some other question.',
      inputSchema: obj({
        id: str('the fact id, from recallMemory'),
        path: str('the note it lives in, from recallMemory'),
        helpful: bool('true if it earned its place, false if it was noise'),
      }, ['id', 'path', 'helpful']),
      execute: ({ id, path, helpful }) => client.feedback(path, id, helpful),
    },

    askNotes: {
      description:
        'Ask the user\'s own knowledge base a question and get an answer with ' +
        'citations. This is their notes and documents, not the web — prefer it over ' +
        'guessing at anything specific to them or their systems.',
      inputSchema: obj({
        question: str('the question, in full'),
        limit: int('how many passages to ground the answer in (default 8)'),
      }, ['question']),
      execute: ({ question, limit }) => client.ask(question, { limit }),
    },
  }

  const chosen = options.include ?? Object.keys(all)
  const tools = {}
  for (const name of chosen) {
    if (!all[name]) {
      throw new Error(`unknown grimoire tool: ${name}`)
    }
    tools[name] = { ...all[name], inputSchema: wrap(all[name].inputSchema) }
  }
  return tools
}

export default grimoireTools
