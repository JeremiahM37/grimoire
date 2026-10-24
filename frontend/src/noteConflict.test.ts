import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ApiError } from './api';
import type { Note } from './types';
import { CONFLICT_MARKER, conflictFrom, keepBoth, keepBothBody, keepMine } from './noteConflict';

const note = (over: Partial<Note> = {}): Note => ({
  path: 'a.md', title: 'A', body: 'base\n', private: false, mtime: 1, hash: 'h1',
  created: '', updated: '', frontmatter: {}, tags: [], encrypted: false, locked: false, trust: '', ...over,
});

test('a 409 with the current note is a conflict carrying that note', () => {
  const mine = note({ body: 'mine\n' });
  const current = note({ body: 'theirs\n', hash: 'h2' });
  const hit = conflictFrom(mine, new ApiError(409, '', 'conflict', '', current));
  assert.equal(hit?.current?.hash, 'h2');
  assert.equal(hit?.mine.body, 'mine\n');
});

test('a 409 for a missing note has no current version', () => {
  const hit = conflictFrom(note(), new ApiError(409, '', 'conflict', '', null));
  assert.ok(hit);
  assert.equal(hit.current, undefined);
});

test('other failures are not conflicts', () => {
  assert.equal(conflictFrom(note(), new ApiError(500, '', 'boom')), undefined);
  assert.equal(conflictFrom(note(), new Error('network down')), undefined);
});

test('keep mine saves over the current hash', () => {
  const next = keepMine(note({ body: 'mine\n' }), note({ hash: 'h2' }));
  assert.equal(next.body, 'mine\n');
  assert.equal(next.hash, 'h2');
});

test('keep mine on a missing note drops the base so the save recreates it', () => {
  assert.equal(keepMine(note()).hash, '');
});

test('keep both appends the local text below the current body with the marker', () => {
  const merged = keepBoth(note({ body: 'my line\n' }), note({ body: 'their line\n', hash: 'h2' }));
  assert.equal(merged.hash, 'h2');
  assert.equal(merged.body, `their line\n\n${CONFLICT_MARKER}\n\nmy line\n`);
  assert.equal(keepBothBody('x\n\n', 'y\n'), `x\n\n${CONFLICT_MARKER}\n\ny\n`);
});
