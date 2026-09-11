import type { APIKey, AuditEvent, Canvas, Connector, ConnectorKind, DocumentRecord, ExtractionResult, ExternalIdentity, Graph, Grant, GrantRequest, Health, Identity, JsonValue, KnowledgeGraph, KnowledgeQueryResult, KnowledgeSource, Note, NoteListItem, Plugin, ReviewQueue, SearchHit, Secret, SecretDetail, Space, TagCount, Task, Template, TrustOverview, UsageReport, User, VaultStatus } from './types';

export class ApiError extends Error {
  constructor(public readonly status: number, public readonly gate: string, message: string) {
    super(message);
    this.name = 'ApiError';
  }
}
export interface RequestOptions extends Omit<RequestInit, 'body'> {
  body?: JsonValue | FormData;
}
export interface ClientOptions {
  fetch?: typeof fetch;
  onSessionExpired?: () => void;
}

export function createClient(options: ClientOptions = {}) {
  const request = options.fetch ?? globalThis.fetch.bind(globalThis);
  return async function api<T>(path: string, init: RequestOptions = {}): Promise<T> {
    const { body, headers: extraHeaders, ...rest } = init;
    const headers = new Headers(extraHeaders);
    const multipart = body instanceof FormData;
    if (!multipart && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    const response = await request(`/api${path}`, {
      ...rest, headers,
      body: body === undefined ? undefined : multipart ? body : JSON.stringify(body),
    });
    if (!response.ok) {
      const gate = response.headers.get('X-Grimoire-Gate') ?? '';
      let message = response.statusText || `Request failed (${response.status})`;
      try {
        const payload: unknown = await response.json();
        if (typeof payload === 'object' && payload !== null && 'detail' in payload && typeof payload.detail === 'string') message = payload.detail;
      } catch { /* Preserve status when a proxy answers with HTML. */ }
      // Admin-token failures are separate from account sessions. Never reload
      // the editor or discard unsaved work for an admin gate refusal.
      if (response.status === 401 && gate !== 'admin') options.onSessionExpired?.();
      throw new ApiError(response.status, gate, message);
    }
    return (response.status === 204 ? null : await response.json()) as T;
  };
}

export const noteURLPath = (path: string): string => path.split('/').map(encodeURIComponent).join('/');
const query = (params: Record<string, string | number | boolean | undefined>) => {
  const q = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) if (value !== undefined && value !== '') q.set(key, String(value));
  return q.size ? `?${q}` : '';
};

export function createGrimoireApi(options: ClientOptions = {}) {
  const request = createClient(options);
  return {
    request,
    me: (signal?: AbortSignal) => request<Identity>('/me', { signal }),
    health: (signal?: AbortSignal) => request<Health>('/health', { signal }),
    notes: (signal?: AbortSignal) => request<NoteListItem[]>('/notes', { signal }),
    note: (path: string, signal?: AbortSignal) => request<Note>(`/notes/${noteURLPath(path)}`, { signal }),
    aliases: (signal?: AbortSignal) => request<Record<string, string>>('/aliases', { signal }),
    tags: (signal?: AbortSignal) => request<TagCount[]>('/tags', { signal }),
    search: (q: string, trusted = false) => request<SearchHit[]>(`/search${query({ q, trusted: trusted || undefined })}`),
    graph: () => request<Graph>('/graph'),
    knowledgeGraph: (params: { seed?: string; depth?: number; limit?: number; relation?: string; q?: string; min_degree?: number; document_visibility?: boolean; drop_noisy?: boolean; chunk_visibility?: boolean } = {}) => request<KnowledgeGraph>(`/knowledge/graph${query(params)}`),
    knowledgeQuery: (body: { question: string; limit?: number; depth?: number; after?: string; before?: string; expand?: boolean }) => request<KnowledgeQueryResult>('/knowledge/query', { method: 'POST', body: body as unknown as JsonValue }),
    knowledgeSource: (path: string) => request<KnowledgeSource>(`/knowledge/source${query({ path })}`),
    extractRelationships: (paths: string[], force = false) => request<{ results: ExtractionResult[] }>('/knowledge/extract', { method: 'POST', body: { paths, force } }),
    documents: () => request<{ documents: DocumentRecord[] }>('/documents'),
    importDocument: (file: File, path?: string) => { const body = new FormData(); body.append('file', file); if (path) body.append('path', path); return request<{ path: string; source_path: string; title: string; format: string }>('/documents/import', { method: 'POST', body }); },
    originalDocument: (path: string) => `/api/documents/original${query({ path })}`,
    refreshDocument: (path: string) => request<DocumentRecord>('/documents/refresh', { method: 'POST', body: { path } }),
    tasks: (done = false) => request<Task[]>(`/tasks${query({ include_done: done || undefined })}`),
    daily: (date?: string) => request<Note>(`/daily${query({ date })}`),
    templates: () => request<Template[]>('/templates'), trash: () => request<JsonValue[]>('/trash'), plugins: () => request<Plugin[]>('/plugins'),
    canvases: () => request<Canvas[]>('/canvas'), canvas: (path: string) => request<Canvas>(`/canvas/${noteURLPath(path)}`), vault: () => request<VaultStatus>('/vault/status'),
    connectors: () => request<Connector[]>('/connectors'), connectorKinds: () => request<ConnectorKind[]>('/connectors/kinds'),
    secrets: () => request<Secret[]>('/secrets'), secretDetails: () => request<{ secrets: SecretDetail[]; needs_attention?: number }>('/secrets/details'), grants: () => request<Grant[]>('/grants'),
    audit: () => request<AuditEvent[]>('/audit'), grantRequests: () => request<{ requests: GrantRequest[]; pending: number }>('/secrets/requests'),
    usage: (since = '30d') => request<UsageReport>(`/usage${query({ since })}`), usageAgents: (since = '30d') => request<JsonValue>(`/usage/agents${query({ since })}`),
    trust: () => request<TrustOverview>('/trust'), reviewQueue: () => request<ReviewQueue>('/stale'),
    spaces: () => request<Space[]>('/spaces'), users: () => request<User[]>('/users'), keys: () => request<APIKey[]>('/keys'), identities: () => request<ExternalIdentity[]>('/identities'), settings: () => request<{ settings: Record<string, JsonValue>; answer_backend?: string }>('/settings'),
    update: (note: Note) => request<Note>(`/notes/${noteURLPath(note.path)}`, { method: 'PUT', body: { body: note.body, frontmatter: { ...note.frontmatter, title: note.title, private: note.private } } }),
    create: (body: JsonValue) => request<Note>('/notes', { method: 'POST', body }), login: (name: string, password: string) => request<unknown>('/auth/login', { method: 'POST', body: { name, password } }), logout: () => request<unknown>('/auth/logout', { method: 'POST' }),
  };
}
