/** Pure model behind the command palette: matching, ranking, sectioning and recents. No React, no DOM. */

export type PaletteNote = { path: string; title?: string };
export type PaletteRowKind = 'note' | 'command' | 'create';
export type PaletteRow = {
  kind: PaletteRowKind;
  /** note: path; command: the command string run by executeCommand; create: the title to use. */
  value: string;
  label: string;
  detail?: string;
  shortcut?: string;
  /** Indexes of label characters to highlight. */
  marks: number[];
};
export type PaletteSection = { title: string; rows: PaletteRow[]; top: number };

export const RECENT_KEY = 'grimoire-recent-notes';
export const RECENT_LIMIT = 20;
export const RECENT_SHOWN = 7;
export const RESULT_LIMIT = 8;
/** A command match at this score or better (prefix, whole word, or exact name) is listed before notes. */
const STRONG_COMMAND = 250;

/**
 * Phrases the keyword router in main.tsx already understands but that have no listed command.
 * They are matched only once typed, so they never clutter the empty palette.
 */
export const TYPED_ONLY_COMMANDS = ['New canvas', "Where this vault's text came from", 'Needs re-checking', 'Open tasks all notes'];

/** Every built-in command string. executeCommand in main.tsx matches on these, so they must not change. */
export const BASE_COMMANDS = [
  'What would the agent see?', 'Agent memories', 'Memory banks', 'Memory use', 'What your agents changed their mind about', 'Everything your agents did', 'Credential requests', 'Unusual reading', 'Rename a tag across all notes', 'Create a plugin', 'Find & replace in note', 'Open random note', 'Toggle sidebar', 'Close split view', 'Export whole vault (.zip)', 'Import vault from .zip', 'Sync now (with configured peer)', 'New unique (Zettel) note', 'Toggle focus mode distraction free', 'Split view open current note on the right', 'Extract selection', 'Merge this note', 'Present this note', 'Edit note properties', 'Pin / unpin note', 'Export note as HTML', 'New note', 'Today daily note', 'Open calendar', 'Browse tags', 'Previous day daily note', 'Next day daily note', "Insert today's date", 'Ask your notes', 'Open graph view', 'Keyboard shortcuts & help', 'Canvas', 'Tasks', 'Version history', 'Trash', 'Vault', 'Connectors', 'AI usage', 'Agent briefing', 'Trust overview', 'Review queue', 'Settings', 'Account & spaces', 'Save current note as template', 'Duplicate this note', 'Encrypt this note', 'Decrypt this note',
];

/** Everyday commands, shown first in the empty palette. Strings must match executeCommand. */
export const EVERYDAY_COMMANDS = [
  'New note', 'Today daily note', 'Ask your notes', 'Open graph view', 'Tasks', 'Open calendar',
  'Browse tags', 'Toggle sidebar', 'Toggle focus mode distraction free', 'Settings', 'Keyboard shortcuts & help',
];

/** Shortcut hints shown beside a command. Keys are the exact command strings. */
export const COMMAND_SHORTCUTS: Record<string, string> = {
  'New note': 'Alt+N',
  'Open graph view': 'Ctrl+G',
  'Toggle sidebar': 'Ctrl+\\',
  'Find & replace in note': 'Ctrl+F',
  'Keyboard shortcuts & help': '?',
};

const STOP_WORDS = new Set(['open', 'show', 'go', 'to', 'the', 'a', 'an', 'this', 'all', 'my', 'of', 'in', 'and', 'how', 'do', 'i']);
const EXACT_COMMAND_BONUS = 150;
const PATH_PENALTY = 150;

/** Command order: everyday commands first, then the rest in their existing order, no duplicates. */
export function orderCommands(commands: readonly string[]): string[] {
  const unique = [...new Set(commands)];
  const everyday = EVERYDAY_COMMANDS.filter(name => unique.includes(name));
  return [...everyday, ...unique.filter(name => !everyday.includes(name))];
}

/** Lower-case, punctuation folded to spaces: the same normalisation executeCommand users see. */
export function normalizeLabel(value: string): string {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim();
}

export function noteTitle(note: PaletteNote): string {
  return note.title || note.path.replace(/\.md$/, '').split('/').pop() || note.path;
}

export function noteFolder(path: string): string {
  const slash = path.lastIndexOf('/');
  return slash > 0 ? path.slice(0, slash) : '';
}

type Hit = { score: number; marks: number[] };
const isWordChar = (c: string) => /[a-z0-9]/.test(c);

/**
 * Score one query against one text. Tiers: exact prefix (400) > word-boundary prefix (300) >
 * contiguous substring (200) > every query word present (90) > characters in order (≈50–100).
 * Shorter texts win ties inside a tier.
 */
export function matchText(query: string, text: string): Hit | null {
  const q = query.trim().toLowerCase();
  const t = text.toLowerCase();
  if (!q) return { score: 0, marks: [] };
  const run = (start: number) => Array.from({ length: q.length }, (_, i) => start + i);
  const length = t.length * 0.01;
  if (t.startsWith(q)) return { score: 400 - length, marks: run(0) };
  for (let at = t.indexOf(q); at > 0; at = t.indexOf(q, at + 1)) {
    if (!isWordChar(t[at - 1] ?? '')) return { score: 300 - length, marks: run(at) };
  }
  const at = t.indexOf(q);
  if (at >= 0) return { score: 200 - length, marks: run(at) };

  const words = q.split(/[^a-z0-9]+/).filter(word => word.length >= 2 && !STOP_WORDS.has(word));
  if (words.length && words.every(word => t.includes(word))) {
    const marks = new Set<number>();
    for (const word of words) for (let i = 0; i < word.length; i++) marks.add(t.indexOf(word) + i);
    return { score: 90 - length, marks: [...marks].sort((a, b) => a - b) };
  }

  const marks: number[] = [];
  let next = 0;
  for (let i = 0; i < t.length && next < q.length; i++) if (t[i] === q[next]) { marks.push(i); next++; }
  if (next < q.length) return null;
  const span = marks[marks.length - 1]! - marks[0]! + 1;
  return { score: Math.max(1, 80 - Math.min(70, span - q.length)) - length, marks };
}

export function buildPalette(input: {
  query: string;
  notes: PaletteNote[];
  commands: readonly string[];
  recents: readonly string[];
}): PaletteSection[] {
  const raw = input.query.trimStart();
  const commandOnly = raw.startsWith('>');
  const query = (commandOnly ? raw.slice(1) : raw).trim();
  const commands = orderCommands(input.commands);
  const noteByPath = new Map(input.notes.map(note => [note.path, note] as const));
  const commandRow = (label: string, hit?: Hit): PaletteRow => ({
    kind: 'command', value: label, label, shortcut: COMMAND_SHORTCUTS[label], marks: hit?.marks ?? [],
  });
  const noteRow = (note: PaletteNote, marks: number[] = []): PaletteRow => {
    const folder = noteFolder(note.path);
    return { kind: 'note', value: note.path, label: noteTitle(note), detail: folder || undefined, marks };
  };

  if (!query) {
    const recent = input.recents
      .map(path => noteByPath.get(path))
      .filter((note): note is PaletteNote => Boolean(note))
      .slice(0, RECENT_SHOWN)
      .map(note => noteRow(note));
    const sections: PaletteSection[] = [];
    if (!commandOnly && recent.length) sections.push({ title: 'Recent', rows: recent, top: 0 });
    sections.push({ title: 'Commands', rows: commands.map(label => commandRow(label)), top: 0 });
    return sections;
  }

  const normalizedQuery = normalizeLabel(query);
  const typedPool = [...commands, ...TYPED_ONLY_COMMANDS.filter(label => !commands.includes(label))];
  const commandHits = typedPool.flatMap(label => {
    const hit = matchText(query, label);
    if (!hit) return [];
    const exact = normalizeLabel(label) === normalizedQuery;
    return [{ label, score: hit.score + (exact ? EXACT_COMMAND_BONUS : 0), marks: hit.marks }];
  }).sort((a, b) => b.score - a.score).slice(0, RESULT_LIMIT);

  const recentRank = new Map(input.recents.slice(0, RECENT_LIMIT).map((path, index) => [path, index] as const));
  const noteHits = commandOnly ? [] : input.notes.flatMap(note => {
    const title = matchText(query, noteTitle(note));
    const path = matchText(query, note.path);
    const best = Math.max(title?.score ?? -Infinity, (path?.score ?? -Infinity) - PATH_PENALTY);
    if (!Number.isFinite(best)) return [];
    const rank = recentRank.get(note.path);
    const boost = rank === undefined ? 0 : Math.max(0, 20 - rank);
    return [{ note, score: best + boost, marks: title?.marks ?? [] }];
  }).sort((a, b) => b.score - a.score || noteTitle(a.note).length - noteTitle(b.note).length).slice(0, RESULT_LIMIT);

  const notesSection: PaletteSection[] = noteHits.length ? [{ title: 'Notes', top: noteHits[0]!.score, rows: noteHits.map(hit => noteRow(hit.note, hit.marks)) }] : [];
  const commandsSection: PaletteSection[] = commandHits.length ? [{ title: 'Commands', top: commandHits[0]!.score, rows: commandHits.map(hit => commandRow(hit.label, hit)) }] : [];
  // A command named by the query (exact name, prefix or whole word) goes first, so typing it runs it.
  const strong = (commandHits[0]?.score ?? 0) >= STRONG_COMMAND;
  const sections: PaletteSection[] = strong ? [...commandsSection, ...notesSection] : [...notesSection, ...commandsSection];

  if (!sections.length && !commandOnly) {
    const title = query;
    sections.push({ title: '', top: 0, rows: [{ kind: 'create', value: title, label: `Create note "${title}"`, marks: [] }] });
  }
  return sections;
}

/** Flat selectable rows in display order (section headings are not selectable). */
export function flattenRows(sections: readonly PaletteSection[]): PaletteRow[] {
  return sections.flatMap(section => section.rows);
}

/** Splits a label into [text, marked] runs so the view can wrap matched characters in <mark>. */
export function markRuns(label: string, marks: readonly number[]): { text: string; marked: boolean }[] {
  const set = new Set(marks);
  const runs: { text: string; marked: boolean }[] = [];
  for (let i = 0; i < label.length; i++) {
    const marked = set.has(i);
    const last = runs[runs.length - 1];
    if (last && last.marked === marked) last.text += label[i];
    else runs.push({ text: label[i]!, marked });
  }
  return runs;
}

function storage(): Storage | undefined {
  try { return globalThis.localStorage; } catch { return undefined; }
}

/** Last opened note paths, newest first. Storage failures (private mode, blocked) read as empty. */
export function readRecents(): string[] {
  try {
    const parsed: unknown = JSON.parse(storage()?.getItem(RECENT_KEY) ?? '[]');
    return Array.isArray(parsed) ? parsed.filter((path): path is string => typeof path === 'string').slice(0, RECENT_LIMIT) : [];
  } catch {
    return [];
  }
}

export function rememberRecent(list: readonly string[], path: string): string[] {
  return [path, ...list.filter(item => item !== path)].slice(0, RECENT_LIMIT);
}

export function writeRecents(list: readonly string[]): void {
  try { storage()?.setItem(RECENT_KEY, JSON.stringify(list)); } catch { /* storage unavailable: recents stay in memory only */ }
}
