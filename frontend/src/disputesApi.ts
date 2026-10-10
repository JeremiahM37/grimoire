// Calls the Disputes panel makes, in one place.
import type { JsonValue } from './types';
import type { Dispute, ResolveBody } from './disputesModel';

export type Request = <T = JsonValue>(path: string, init?: { method?: string; body?: JsonValue | FormData; signal?: AbortSignal }) => Promise<T>;

export interface ResolveResponse {
  id: string;
  path: string;
  resolution: string;
  stands: string;
  superseded: string[];
}

export function createDisputesApi(request: Request) {
  return {
    list: (signal?: AbortSignal) => request<Dispute[]>('/memory/disputes', { signal }),
    resolve: (body: ResolveBody) => request<ResolveResponse>('/memory/disputes/resolve', { method: 'POST', body: body as unknown as JsonValue }),
  };
}
export type DisputesApi = ReturnType<typeof createDisputesApi>;
