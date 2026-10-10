import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BASE_COMMANDS, buildPalette, flattenRows, markRuns, matchText, orderCommands, readRecents, rememberRecent, writeRecents } from './paletteModel';

const notes = Array.from({ length: 25 }, (_, i) => ({ path: `plain-${String(i).padStart(2, '0')}.md`, title: `Plain note ${i}` }));
notes.push({ path: 'projects/palette-target.md', title: 'Palette target' });

test('empty query lists recent notes newest first, capped at seven, then everyday commands', () => {
  const recents = ['projects/palette-target.md', ...notes.slice(0, 9).map(n => n.path)];
  const sections = buildPalette({ query: '', notes, commands: BASE_COMMANDS, recents });
  assert.deepEqual(sections.map(s => s.title), ['Recent', 'Commands']);
  const recent = sections[0]!.rows;
  assert.equal(recent.length, 7);
  assert.equal(recent[0]!.value, 'projects/palette-target.md');
  assert.equal(recent[0]!.detail, 'projects');
  const commands = sections[1]!.rows.map(r => r.value);
  assert.deepEqual(commands.slice(0, 11), ['New note', 'Today daily note', 'Ask your notes', 'Open graph view', 'Tasks', 'Open calendar', 'Browse tags', 'Toggle sidebar', 'Toggle focus mode distraction free', 'Settings', 'Keyboard shortcuts & help']);
  assert.equal(new Set(commands).size, commands.length);
  assert.equal(sections[1]!.rows.find(r => r.value === 'New note')!.shortcut, 'Alt+N');
});

test('recents that no longer exist are skipped', () => {
  const sections = buildPalette({ query: '', notes, commands: BASE_COMMANDS, recents: ['gone.md', 'plain-03.md'] });
  assert.deepEqual(sections[0]!.rows.map(r => r.value), ['plain-03.md']);
});

test('a note outside the first twenty is found by title', () => {
  const rows = flattenRows(buildPalette({ query: 'palette target', notes, commands: BASE_COMMANDS, recents: [] }));
  assert.equal(rows[0]!.kind, 'note');
  assert.equal(rows[0]!.value, 'projects/palette-target.md');
});

test('a note matched by path alone still appears', () => {
  const rows = flattenRows(buildPalette({ query: 'projects palette', notes, commands: BASE_COMMANDS, recents: [] }));
  assert.ok(rows.some(r => r.kind === 'note' && r.value === 'projects/palette-target.md'));
});

test('a leading > shows commands only', () => {
  const sections = buildPalette({ query: '>graph', notes, commands: BASE_COMMANDS, recents: [] });
  assert.deepEqual(sections.map(s => s.title), ['Commands']);
  assert.ok(flattenRows(sections).every(r => r.kind === 'command'));
  assert.equal(flattenRows(sections)[0]!.value, 'Open graph view');
});

test('a full command name is selected before a note that contains it', () => {
  const rows = flattenRows(buildPalette({ query: 'settings', notes: [...notes, { path: 'settings-tips.md', title: 'Settings tips' }], commands: BASE_COMMANDS, recents: [] }));
  assert.equal(rows[0]!.kind, 'command');
  assert.equal(rows[0]!.value, 'Settings');
});

test('keyword phrases still reach their command', () => {
  const rows = flattenRows(buildPalette({ query: 'pin unpin this note', notes: [], commands: BASE_COMMANDS, recents: [] }));
  assert.equal(rows[0]!.value, 'Pin / unpin note');
});

test('no match offers one create row with the typed title', () => {
  const sections = buildPalette({ query: 'Zzqx Quux', notes, commands: BASE_COMMANDS, recents: [] });
  const rows = flattenRows(sections);
  assert.equal(rows.length, 1);
  assert.equal(rows[0]!.kind, 'create');
  assert.equal(rows[0]!.value, 'Zzqx Quux');
  assert.equal(rows[0]!.label, 'Create note "Zzqx Quux"');
});

test('no create row in command-only mode', () => {
  assert.deepEqual(buildPalette({ query: '>zzqx', notes, commands: BASE_COMMANDS, recents: [] }), []);
});

test('match tiers rank prefix > word boundary > substring > subsequence', () => {
  const prefix = matchText('gra', 'Graph view')!.score;
  const word = matchText('view', 'Open graph view')!.score;
  const substring = matchText('raph', 'Open graph view')!.score;
  const subsequence = matchText('ogv', 'Open graph view')!.score;
  assert.ok(prefix > word && word > substring && substring > subsequence && subsequence > 0);
  assert.equal(matchText('xyz', 'Open graph view'), null);
});

test('shorter titles win ties inside a tier', () => {
  const rows = flattenRows(buildPalette({ query: 'draft', notes: [{ path: 'a.md', title: 'Draft ideas for later' }, { path: 'b.md', title: 'Draft' }], commands: [], recents: [] }));
  assert.deepEqual(rows.map(r => r.value), ['b.md', 'a.md']);
});

test('a recent note gets a small boost that only reorders near ties', () => {
  const pair = [{ path: 'x.md', title: 'Meeting notes' }, { path: 'y.md', title: 'Meeting notes' }];
  const rows = flattenRows(buildPalette({ query: 'meeting', notes: pair, commands: [], recents: ['y.md'] }));
  assert.deepEqual(rows.map(r => r.value), ['y.md', 'x.md']);
});

test('matched characters are marked as runs', () => {
  assert.deepEqual(markRuns('Open graph', [0, 1, 5]).map(r => [r.text, r.marked]), [['Op', true], ['en ', false], ['g', true], ['raph', false]]);
  const hit = matchText('ogr', 'Open graph view')!;
  // no prefix, word or substring hit: characters matched in order
  assert.deepEqual(hit.marks, [0, 5, 6]);
});

test('orderCommands puts everyday commands first without duplicates', () => {
  assert.deepEqual(orderCommands(['Tasks', 'Other', 'Settings', 'Tasks']), ['Tasks', 'Settings', 'Other']);
});

test('recents keep the newest twenty without duplicates', () => {
  let list: string[] = [];
  for (let i = 0; i < 25; i++) list = rememberRecent(list, `n${i}.md`);
  list = rememberRecent(list, 'n20.md');
  assert.equal(list.length, 20);
  assert.equal(list[0], 'n20.md');
  assert.equal(new Set(list).size, 20);
});

test('recents survive a storage that throws or is missing', () => {
  assert.deepEqual(readRecents(), []);
  assert.doesNotThrow(() => writeRecents(['a.md']));
});

test('a typed-only phrase beats a note with the same title', () => {
  const rows = flattenRows(buildPalette({ query: 'new canvas', notes: [{ path: 'new-canvas.md', title: 'new canvas' }], commands: BASE_COMMANDS, recents: [] }));
  assert.equal(rows[0]!.kind, 'command');
  assert.equal(rows[0]!.value, 'New canvas');
  assert.equal(rows[1]!.value, 'new-canvas.md');
});

test('typed-only phrases stay out of the empty palette', () => {
  const values = flattenRows(buildPalette({ query: '', notes: [], commands: BASE_COMMANDS, recents: [] })).map(r => r.value);
  assert.ok(!values.includes('New canvas'));
});

test('the open note is left out of Recent, so Enter switches to the previous one', () => {
  const notes = [{ path: 'a.md', title: 'Alpha' }, { path: 'b.md', title: 'Beta' }];
  const sections = buildPalette({ query: '', notes, commands: BASE_COMMANDS, recents: ['a.md', 'b.md'], current: 'a.md' });
  assert.equal(sections[0]!.title, 'Recent');
  assert.deepEqual(sections[0]!.rows.map(row => row.value), ['b.md']);
});

test('a command shows a readable label, runs its command string, and matches on either', () => {
  for (const query of ['focus', 'distraction']) {
    const row = flattenRows(buildPalette({ query: '>' + query, notes: [], commands: BASE_COMMANDS, recents: [] }))[0]!;
    assert.equal(row.label, 'Focus mode', query);
    assert.equal(row.value, 'Toggle focus mode distraction free', query);
  }
  const today = flattenRows(buildPalette({ query: "today's", notes: [], commands: BASE_COMMANDS, recents: [] }))[0]!;
  assert.equal(today.value, 'Today daily note');
  assert.deepEqual(today.marks, [0, 1, 2, 3, 4, 5, 6]);
});
