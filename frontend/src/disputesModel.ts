// What the Disputes panel shows, and the rules for building a resolution. Pure
// so it can be tested without a browser. The shapes are the ones
// docs/DISPUTES.md lists for GET /api/memory/disputes and its resolve route.

export interface DisputeSide {
  id: string;
  path: string;
  text: string;
  agent?: string;
  stamp?: string;
  /** 'human', 'agent' or 'pulled' (text other people can write). */
  authority: string;
  evidence: string[];
}

export interface Dispute {
  id: string;
  path: string;
  disputed: DisputeSide;
  challengers: DisputeSide[];
}

export type Resolution = 'keep' | 'accept_challenger' | 'merge';

export interface ResolveBody {
  id: string;
  path?: string;
  resolution: Resolution;
  text?: string;
  challenger?: string;
}

/** Longest merged fact the server accepts. */
export const MAX_MERGE_TEXT = 20000;

export const RESOLUTION_LABEL: Record<Resolution, string> = {
  keep: 'Keep my fact',
  accept_challenger: "Accept the agent's",
  merge: 'Write a merged fact',
};

const AUTHORITY_WORDS: Record<string, string> = {
  human: 'a person',
  agent: 'an agent',
  pulled: 'imported text',
};

/** Who asserted a side, in words a person reads. */
export function authorityText(authority: string): string {
  return AUTHORITY_WORDS[authority] ?? authority;
}

/** "2 facts contest this" / "1 fact contests this". */
export function contestText(d: Dispute): string {
  const n = d.challengers.length;
  return `${n} ${n === 1 ? 'fact contests' : 'facts contest'} this`;
}

export type ResolveResult = { ok: true; body: ResolveBody } | { ok: false; error: string };

/**
 * Builds the request for one resolution, or says why it cannot be built. The
 * checks mirror the server's, so a person sees the reason before a round trip.
 */
export function resolveBody(
  d: Dispute,
  choice: Resolution,
  options: { text?: string; challenger?: string } = {},
): ResolveResult {
  const body: ResolveBody = { id: d.id, resolution: choice };
  if (d.path) body.path = d.path;
  if (choice === 'merge') {
    const text = (options.text ?? '').trim();
    if (!text) return { ok: false, error: 'Write the fact as it should be stored.' };
    if (text.length > MAX_MERGE_TEXT) return { ok: false, error: `Keep it under ${MAX_MERGE_TEXT} characters.` };
    body.text = text;
    return { ok: true, body };
  }
  if (choice === 'accept_challenger') {
    const pick = options.challenger || (d.challengers.length === 1 ? d.challengers[0]?.id : undefined);
    if (!pick) return { ok: false, error: 'Choose which contesting fact to accept.' };
    if (!d.challengers.some(c => c.id === pick)) return { ok: false, error: 'That fact is not contesting this one.' };
    body.challenger = pick;
    return { ok: true, body };
  }
  return { ok: true, body };
}
