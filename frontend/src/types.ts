export type JsonPrimitive = string | number | boolean | null;
export type JsonValue = JsonPrimitive | JsonValue[] | { [key: string]: JsonValue };

// These are API response shapes (internal/api/api.go), not vault.Note's
// internal fields. List rows intentionally do not contain note bodies.
export interface NoteListItem {
  path: string;
  title: string;
  updated: string;
  private: boolean;
  pinned: boolean;
  untrusted?: boolean;
}
export interface Note {
  path: string;
  title: string;
  body: string;
  private: boolean;
  mtime: number;
  hash: string;
  created: string;
  updated: string;
  frontmatter: Record<string, JsonValue>;
  tags: string[];
  encrypted: boolean;
  locked: boolean;
  origin?: string;
  trust: string;
  backlinks?: Link[];
  links?: Link[];
}
export interface Link { path: string; title: string; alias?: string }
export interface Identity {
  multi_user: boolean;
  anonymous: boolean;
  admin: boolean;
  name: string;
  user?: { id: string; name: string; display: string; role: string; created: string };
  spaces?: { id: string; name: string; prefix: string; kind: string; writable: boolean }[];
}
export interface TagCount { tag: string; c: number }
export interface Health {
  ok: boolean;
  version: string;
  build: { revision?: string; modified?: boolean; [key: string]: JsonValue | undefined };
  vault: string;
  notes: number;
  tags: number;
  unresolved_links: number;
  embedder: string;
  rev: string;
}
export interface SearchHit { path: string; title: string; snippet: string; body?: string; origin?: string; trust: string }
export interface Graph { nodes: { id: string; title: string }[]; edges: { src: string; dst: string }[]; unresolved: string[] }
export type KnowledgeNodeKind = 'entity' | 'document' | 'chunk';
export interface KnowledgeEvidence { path: string; title: string; text: string; origin: string; trust: string }
export interface KnowledgeNode { id: string; label: string; kind: KnowledgeNodeKind; path?: string }
export interface KnowledgeEdge { id: string; source: string; target: string; relation: string; evidence: KnowledgeEvidence[] }
export interface KnowledgeGraph { revision: number; nodes: KnowledgeNode[]; edges: KnowledgeEdge[]; truncated: boolean }
export interface KnowledgeQueryResult { answer: string; citations: Array<KnowledgeEvidence & { id: string }>; graph: KnowledgeGraph; revision: number }
export interface KnowledgeSource extends KnowledgeEvidence { total?: number; truncated?: boolean }
export interface DocumentRecord { path: string; source_path: string; title: string; format: string; status: string; error?: string }
export interface ExtractionResult { path: string; status: 'indexed' | 'cached' | 'error'; triples: number; error?: string }
export interface Task { path: string; title: string; line: number; text: string; done: boolean; section?: string; trust: string; origin?: string }
export interface Canvas { path: string; nodes: CanvasNode[]; edges: CanvasEdge[] }
export interface CanvasNode { id: string; type?: string; text?: string; x: number; y: number; width?: number; height?: number; file?: string }
export interface CanvasEdge { id: string; fromNode: string; toNode: string; label?: string }
export interface Template { name: string; path?: string; title?: string }
export interface VaultStatus { initialized?: boolean; unlocked?: boolean; [key: string]: JsonValue | undefined }
export interface Secret { name: string }
export interface SecretDetail { name: string; status?: 'expired' | 'expiring' | 'stale' | string; expires?: string; uses?: number; versions?: number; note?: string }
export interface Grant { token: string; secret: string; grantee: string; scope?: string; expires_at?: number; max_uses?: number; uses?: number }
export interface GrantRequest { id: string; secret: string; grantee: string; scope?: string; state: string; created?: string; reason?: string; expires_at?: number }
export interface AuditEvent { ts?: string; action: string; secret?: string; detail?: string }
export interface Plugin { name: string; enabled: boolean; title?: string; description?: string; source?: string; version?: string; client_url?: string; styles_url?: string; [key: string]: JsonValue | undefined }
export interface ConnectorField { name: string; label?: string; placeholder?: string; help?: string; required?: boolean }
export interface ConnectorKind { kind: string; name?: string; help: string; secret_help?: string; default_prefix?: string; fields?: ConnectorField[] }
export interface Connector { id: string; name: string; kind: string; prefix: string; interval: number; enabled?: boolean; docs?: number; last_ok?: boolean; last_error?: string; last_run?: string }
export interface UsageGroup { key?: string; provider?: string; model?: string; calls?: number; input_tokens?: number; output_tokens?: number; cost?: number }
export interface UsageSummary { calls?: number; input_tokens?: number; output_tokens?: number; total_tokens?: number; cost?: number; [key: string]: JsonValue | undefined }
export interface UsageReport { window?: string; summary?: UsageSummary; recent?: { at?: string; provider?: string; model?: string; input_tokens?: number; output_tokens?: number; cost?: number }[]; by_provider?: UsageGroup[]; by_model?: UsageGroup[] }
export interface Space { id: string; name: string; prefix: string; kind: string; owner?: string; created?: string; writable: boolean }
export interface User { id: string; name: string; display?: string; role: string; created?: string }
export interface APIKey { id: string; label?: string; created?: string; last_used?: string }
export interface ExternalIdentity { source: string; external: string; user?: string; user_id?: string }
export interface TrustOverview { trusted: number; untrusted: number; enabled: boolean; origins: { origin: string; source: string; notes: number }[] }
export interface StaleNote { path: string; title: string; age_days: number; verified: boolean; inbound: number; score: number; origin?: string }
export interface ReviewQueue { notes: StaleNote[]; total: number; threshold: number; reviewed: number }
