// Bank templates in the server's manifest shape (go/internal/bank/templates.go).
//
// The server's catalogue (GET /api/bank-templates) is what the panel offers.
// The copies below are only for an older server that has no template routes:
// there only the profile fields apply (no mental models, no directive
// routes), so their settings stay within what such a server accepts.
// Keep the text in step with go/internal/bank/templates.go.

export interface TemplateMentalModel {
  id: string; name: string; question: string; tags?: string[]; refresh?: string; max_tokens?: number; budget?: string; fact_types?: string[];
}
export interface TemplateDirective { name?: string; text: string; tags?: string[]; priority?: number; inactive?: boolean }
export interface Manifest {
  version?: string;
  bank?: {
    name?: string; mission?: string; retain_mission?: string; disposition?: { skepticism: number; literalism: number; empathy: number };
    tags?: string[]; config?: Record<string, string>;
  };
  mental_models?: TemplateMentalModel[];
  directives?: TemplateDirective[];
}
export interface BankTemplate { id: string; name: string; description: string; manifest: Manifest }

export const builtinTemplates: BankTemplate[] = [
  { id: 'assistant', name: 'Personal assistant',
    description: 'A general assistant that remembers the user: who they are, what they prefer, what they are working on and what they asked for.',
    manifest: { version: '1', bank: {
      mission: 'Help the user by remembering who they are, the people in their life, their preferences, commitments and ongoing plans.',
      retain_mission: 'Keep facts about the user and the people around them, their preferences, plans, commitments, routines and decisions. Skip small talk.',
      disposition: { skepticism: 3, literalism: 3, empathy: 4 }, config: { consolidation: 'auto' } } } },
  { id: 'coding-agent', name: 'Coding agent',
    description: "Memory for an agent working in one codebase: decisions and their reasons, conventions, the developer's preferences and recurring review feedback.",
    manifest: { version: '1', bank: {
      mission: "Support work on this codebase: recall the project's decisions, conventions and constraints, and how the developer likes things done.",
      retain_mission: "Keep technical decisions and their reasons, conventions, architecture, build and test commands, constraints, bugs found and their fixes, and the developer's stated preferences. Skip routine tool output.",
      disposition: { skepticism: 3, literalism: 5, empathy: 2 }, config: { consolidation: 'auto' } },
    directives: [{ name: 'Cite decisions', text: 'When an answer rests on a past decision, say when it was made and why.' }] } },
  { id: 'support', name: 'Customer support',
    description: "Memory for a support agent: each customer's history, issues and how they were resolved, and how the customer felt about it.",
    manifest: { version: '1', bank: {
      mission: 'Help support staff serve each customer well, knowing their history, open issues and what resolved similar problems before.',
      retain_mission: 'Keep customer details, products and plans, reported issues with dates, what was tried, what resolved them, and how the customer felt. Skip pleasantries.',
      disposition: { skepticism: 2, literalism: 3, empathy: 5 }, config: { consolidation: 'auto' } },
    directives: [{ name: 'No internal notes', text: "Never repeat internal-only notes or other customers' details in an answer meant for a customer." }] } },
  { id: 'research', name: 'Research assistant',
    description: "Memory for research: sources, claims with where they came from, open questions, and how the user's thinking developed.",
    manifest: { version: '1', bank: {
      mission: 'Support research by keeping track of sources, the claims they make, open questions and how conclusions developed.',
      retain_mission: "Keep claims with their sources, numbers with units and dates, methods, open questions, disagreements between sources, and the user's interests. Skip chatter.",
      disposition: { skepticism: 4, literalism: 4, empathy: 2 }, config: { consolidation: 'auto' } },
    directives: [{ name: 'Attribute claims', text: 'Attribute every claim to its source, and say when sources disagree.' }] } },
  { id: 'plain-retrieval', name: 'Plain retrieval',
    description: 'A bank with no model in the loop: content is stored chunk by chunk and recalled by meaning, words and time.',
    manifest: { version: '1', bank: { config: { retain_extraction_mode: 'chunks', consolidation: 'off', enable_graph: 'false' } } } },
];
