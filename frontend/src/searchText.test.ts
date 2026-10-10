import { test } from 'node:test';
import assert from 'node:assert/strict';
import { plainSnippet, queryTerms } from './searchText';

test('query terms ignore tag filters, short words and repeats', () => {
  assert.deepEqual(queryTerms('tag:work Quarry a quarry "river"'), ['quarry', 'river']);
  assert.deepEqual(queryTerms('   '), []);
});

test('snippets become plain text with the server brackets around matches removed', () => {
  const raw = '# Alpha [Quarry]\n\nThe **[quarry]** is <b>near</b> the   river [[Link]] …';
  assert.equal(plainSnippet(raw, ['quarry']), '# Alpha Quarry The quarry is near the river [[Link]] …');
});
