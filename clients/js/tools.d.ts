import type { Grimoire } from './index.js'

/** One tool, in the shape `generateText({ tools })` accepts. */
export interface GrimoireTool<Args = Record<string, unknown>, Result = unknown> {
  description: string
  /** JSON Schema, or whatever `options.jsonSchema` wrapped it into. */
  inputSchema: unknown
  execute: (args: Args) => Promise<Result>
}

export type GrimoireToolName =
  | 'recallMemory'
  | 'rememberFact'
  | 'forgetFact'
  | 'memoryGraph'
  | 'rateMemory'
  | 'askNotes'

export interface GrimoireToolsOptions {
  /**
   * The AI SDK's `jsonSchema` helper. Without it `inputSchema` is a plain
   * JSON Schema object, which most other frameworks accept as-is.
   */
  jsonSchema?: (schema: object) => unknown
  /** Only build these tools. Unknown names throw. */
  include?: GrimoireToolName[]
}

export function grimoireTools(
  client: Grimoire,
  options?: GrimoireToolsOptions,
): Record<GrimoireToolName, GrimoireTool>

export default grimoireTools
