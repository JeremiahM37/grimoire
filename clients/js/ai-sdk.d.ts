import type { Grimoire } from './index.js'

export const AGENT_AUTHORED: 'agent-authored'

/** The fence preamble. Include it in the system prompt when `isFenced` is true for recalled text. */
export const PREAMBLE: string

export function neutralize(text: string): string
export function fence(text: string, options?: { origin?: string; n?: number }): string
export function isFenced(text: string): boolean
export function textOf(memory: { text: string; trust?: string; origin?: string }, n?: number): string

export interface JsonSchemaObject {
  type: 'object'
  properties: Record<string, unknown>
  required: string[]
  additionalProperties: false
}

/** One AI SDK tool: `generateText({ tools: { name: tool } })` accepts it. */
export interface AiSdkTool<Args, Result = string> {
  description: string
  parameters: JsonSchemaObject
  execute: (args: Args) => Promise<Result>
}

export interface AiSdkTools {
  remember: AiSdkTool<{ text: string; topic?: string }>
  recall: AiSdkTool<{ query: string; limit?: number }>
  search_notes: AiSdkTool<{ query: string; limit?: number }>
}

export interface AiSdkToolOptions {
  /** Named on every write, so recall can say which agent recorded a fact. */
  agent?: string
  /** Default maximum results for recall and search. */
  limit?: number
}

export function aiSdkTools(client: Grimoire, options?: AiSdkToolOptions): AiSdkTools

export interface GenerateParams {
  prompt?: string
  system?: string
  messages?: Array<{ role: string; content: unknown }>
  [key: string]: unknown
}

export interface WithMemoryOptions {
  client: Grimoire
  agent?: string
  /** The session the exchange is retained in, and the recall is scoped to. */
  session?: string
  limit?: number
  /** Recall before the call. Default true. */
  inject?: boolean
  /** Retain the exchange after the call. Default true. */
  retain?: boolean
  onError?: (err: unknown) => void
}

export function withGrimoireMemory<P extends GenerateParams, R extends { text?: string }>(
  generate: (params: P) => Promise<R>,
  options: WithMemoryOptions,
): (params?: P) => Promise<R>

export default aiSdkTools
