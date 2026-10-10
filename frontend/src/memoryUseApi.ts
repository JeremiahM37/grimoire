// Calls the Memory use panel makes, in one place.
import type { JsonValue } from './types';
import type { AdherenceResponse, CardResponse, RulesResponse, SummaryResponse } from './memoryUseModel';

export type Request = <T = JsonValue>(path: string, init?: { method?: string; body?: JsonValue | FormData; signal?: AbortSignal }) => Promise<T>;

export function createMemoryUseApi(request: Request) {
  return {
    summary: (days: number, signal?: AbortSignal) => request<SummaryResponse>(`/memory/trace/summary?days=${days}`, { signal }),
    adherence: (days: number, signal?: AbortSignal) => request<AdherenceResponse>(`/memory/adherence?days=${days}`, { signal }),
    rules: (signal?: AbortSignal) => request<RulesResponse>('/memory/rules', { signal }),
    card: (target: string, days: number, signal?: AbortSignal) => request<CardResponse>(`/memory/trace?target=${encodeURIComponent(target)}&days=${days}`, { signal }),
  };
}
export type MemoryUseApi = ReturnType<typeof createMemoryUseApi>;
