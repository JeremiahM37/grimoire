import { test } from 'node:test';
import assert from 'node:assert/strict';
import { plainSnippet, queryTerms } from './searchText';

test('query terms ignore tag filters, short words and repeats', () => {
  assert.deepEqual(queryTerms('tag:work Quarry a quarry "river"'), ['quarry', 'river']);
  assert.deepEqual(queryTerms('   '), []);
});

test('snippets become plain text with the server brackets around matches removed', () => {
  const raw = '# Alpha [Quarry]\n\nThe **[quarry]** is <b>near</b> the   river [[Link]] …';
  assert.equal(plainSnippet(raw, ['quarry']), 'Alpha Quarry The quarry is near the river [[Link]] …');
});

test('block markdown is dropped from a one-line excerpt', () => {
  assert.equal(plainSnippet('port:: 8443 ## Rolling back a bad [deploy] 1. Pin the tag', ['deploy']), 'port:: 8443 Rolling back a bad deploy 1. Pin the tag');
  assert.equal(plainSnippet('> quoted [deploy] line\n- [ ] a task\n- an item', ['deploy']), 'quoted deploy line a task an item');
  assert.equal(plainSnippet('| Signal | Threshold | Pages |\n|---|---|---|', []), 'Signal · Threshold · Pages');
  // a hash inside a word is a tag or an anchor, not a heading
  assert.equal(plainSnippet('see #ops and C# code', []), 'see #ops and C# code');
});
