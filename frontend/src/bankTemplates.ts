// Built-in bank templates, used when the server offers no catalogue of its own.
// Keep in step with go/cmd/grimoire/cli_bank_next.go

export interface TemplateMentalModel { id: string; name: string; source_query: string; tags?: string[]; max_tokens?: number; trigger?: { refresh_after_consolidation?: boolean } }
export interface BankTemplate {
  id: string; name: string; description: string;
  manifest: {
    bank?: { name?: string; mission?: string; retain_mission?: string; disposition?: { skepticism: number; literalism: number; empathy: number }; directives?: { text: string }[]; config?: Record<string, string> };
    mental_models?: TemplateMentalModel[];
  };
}

const afterConsolidation = { refresh_after_consolidation: true };

export const builtinTemplates: BankTemplate[] = [
  { id: 'assistant', name: 'Personal assistant', description: "Remembers a person's preferences, plans, people and history.",
    manifest: { bank: {
      mission: 'Remember what matters about the user — preferences, plans, commitments, relationships and history — so future conversations pick up where the last one left off.',
      retain_mission: 'Keep durable facts about the user and the people and plans in their life. Skip greetings, small talk and anything only true for the moment.',
      disposition: { skepticism: 3, literalism: 3, empathy: 4 } } } },
  { id: 'coding-agent', name: 'Coding agent', description: 'One bank per repository: decisions, conventions, history and pitfalls.',
    manifest: { bank: {
      mission: 'Record what a coding agent working in this repository needs to know: decisions and their reasons, conventions, history, and pitfalls.',
      retain_mission: "Extract technical decisions, architectural choices, conventions, gotchas, and the developer's preferences, with the reasons behind them. Ignore routine chatter and step-by-step tool output.",
      disposition: { skepticism: 3, literalism: 5, empathy: 2 } },
    mental_models: [
      { id: 'project-context', name: 'Project context', source_query: 'What is this project, how is it structured, and what are its key technical decisions and constraints?', trigger: afterConsolidation },
      { id: 'developer-preferences', name: 'Developer preferences', source_query: 'What does the developer prefer and dislike in code style, tools, workflow and communication?', trigger: afterConsolidation },
      { id: 'review-patterns', name: 'Review patterns', source_query: 'What kinds of mistakes and review feedback come up repeatedly in this repository?', trigger: afterConsolidation },
    ] } },
  { id: 'support', name: 'Customer support', description: 'Issues, resolutions and how each customer feels.',
    manifest: { bank: {
      mission: "Help support conversations go well: each customer's issues, what resolved them, and how they feel about it.",
      retain_mission: "Keep reported problems, their resolutions, product facts and the customer's sentiment and preferences.",
      disposition: { skepticism: 3, literalism: 3, empathy: 5 } } } },
  { id: 'research', name: 'Research assistant', description: "Sourced, durable knowledge and the user's interests.",
    manifest: { bank: {
      mission: "Build up sourced, durable knowledge on the user's research interests and keep track of where each claim came from.",
      retain_mission: "Keep claims with their sources, findings, open questions and the user's interests. Note when sources disagree.",
      disposition: { skepticism: 4, literalism: 4, empathy: 3 } } } },
  { id: 'plain-retrieval', name: 'Plain retrieval', description: 'No model at retain: stores each chunk as written and recalls by meaning and words.',
    manifest: { bank: { config: { retain_extraction_mode: 'chunks', enable_graph: 'false', enable_temporal: 'false', enable_reranking: 'false' } } } },
];
