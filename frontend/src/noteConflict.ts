import { ApiError } from './api';
import type { Note } from './types';

// Marks where a "Keep both" merge starts, so the user can find and tidy it.
export const CONFLICT_MARKER = '<!-- conflict: your unsaved edit -->';

// A save the server refused with 409 because the note changed since the
// editor loaded it. current is the server's version, or undefined when the
// note no longer exists at that path.
export interface NoteConflict {
  mine: Note;
  current?: Note;
}

export function isSaveConflict(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409;
}

export function conflictFrom(mine: Note, error: unknown): NoteConflict | undefined {
  if (!isSaveConflict(error)) return undefined;
  const current = (error as ApiError).current;
  return { mine, current: current && typeof current === 'object' ? (current as Note) : undefined };
}

export function keepBothBody(current: string, mine: string): string {
  return `${current.trimEnd()}\n\n${CONFLICT_MARKER}\n\n${mine.trimEnd()}\n`;
}

// Keep mine: the local body is saved over the current version. Using the
// current hash as the base makes the next save succeed.
export function keepMine(mine: Note, current?: Note): Note {
  return { ...mine, hash: current?.hash ?? '' };
}

// Keep both: the current body with the local edit appended below it.
export function keepBoth(mine: Note, current: Note): Note {
  return { ...mine, body: keepBothBody(current.body, mine.body), hash: current.hash };
}
