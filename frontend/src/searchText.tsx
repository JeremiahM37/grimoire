import type { ReactNode } from 'react';
import type { NoteListItem } from './types';

/** A sidebar row from a content search: the server's snippet rides along. */
export type SearchRow = NoteListItem & { snippet?: string };

/** Query words that can be highlighted: whitespace-split, no `tag:` filters, two characters or more. */
export function queryTerms(query: string): string[] {
  const terms = query.split(/\s+/)
    .filter(token => token && !/^tag:/i.test(token))
    .map(token => token.replace(/^"+|"+$/g, '').toLowerCase())
    .filter(token => token.length >= 2);
  return [...new Set(terms)];
}

/**
 * The server snippet is markup-ish (it brackets matched words as [word] and may keep
 * markdown or HTML). Reduce it to plain text: tags stripped, markers dropped, whitespace collapsed.
 */
export function plainSnippet(raw: string, terms: string[]): string {
  const text = raw
    .replace(/<[^>]*>/g, ' ')
    .replace(/\[([^[\]\n]+)\]/g, (whole, inner: string) => terms.some(term => inner.toLowerCase().includes(term)) ? inner : whole)
    .replace(/\*\*|__|`/g, '')
    // block syntax means nothing in a one-line excerpt: heading hashes, quote marks, list and task markers, table rules and bars
    .replace(/(^|\s)#{1,6}\s+/g, '$1')
    .replace(/(^|\s)>\s+/g, '$1')
    .replace(/(^|\s)[-*+]\s+(\[[ xX]\]\s+)?/g, '$1')
    .replace(/[-:]{3,}/g, ' ')
    .replace(/\|/g, ' · ');
  return text.replace(/\s+/g, ' ').trim().replace(/(· )+/g, '· ').replace(/^(· ?)+|( ?·)+$/g, '').trim();
}

const escapeRegExp = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/** Render text as React nodes with every query term wrapped in <mark>. Plain text only, never HTML. */
export function highlight(text: string, terms: string[]): ReactNode {
  if (!terms.length || !text) return text;
  const pattern = new RegExp('(' + [...terms].sort((a, b) => b.length - a.length).map(escapeRegExp).join('|') + ')', 'gi');
  return text.split(pattern).map((part, index) => index % 2 ? <mark key={index}>{part}</mark> : part);
}
