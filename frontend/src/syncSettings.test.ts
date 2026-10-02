import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { SyncSettingsView, type SyncSettingsViewProps } from './SyncSettings';
import { FORGET_WARNING, createsBackup, deviceIssueText, initialMode, isICloudPath, problemText, relativeTime, setupProblem, statusSummary, tildify, type FolderSyncStatus } from './syncSettings';

const NOW = Date.UTC(2026, 9, 2, 12, 0, 0);
const noop = () => {};
const status = (over: Partial<FolderSyncStatus> = {}): FolderSyncStatus => ({
  enabled: true, folder: '/home/jo/Dropbox/Grimoire', device_id: 'a', device_name: 'Laptop', interval: 60,
  running: false, last_sync: NOW - 2 * 60_000, last_attempt: NOW - 2 * 60_000, error: null,
  stats: { uploaded: 0, downloaded: 0, deleted: 0, merged: 0, conflicts: 0, waiting: 0, failed: 0, devices: 3 },
  devices: [
    { id: 'a', name: 'Laptop', last_seen: NOW - 2 * 60_000, self: true },
    { id: 'b', name: 'Desktop', last_seen: NOW - 3 * 3600_000, self: false },
    { id: 'c', name: 'Old Mac', last_seen: NOW - 5 * 86400_000, self: false, issue: 'not_downloaded' },
  ],
  icloud: false, suggestions: [], peer: null, home: '/home/jo', ...over,
});
const render = (over: Partial<SyncSettingsViewProps>) => renderToStaticMarkup(createElement(SyncSettingsView, {
  mode: 'folder', form: { folder: '', passphrase: '', confirm: '', deviceName: '' }, now: NOW,
  onMode: noop, onForm: noop, onSetup: noop, onSyncNow: noop, onPeerSyncNow: noop, onTurnOff: noop,
  onBrowse: noop, onCloseBrowse: noop, onLoadDeleted: noop, onRestore: noop, ...over,
}));
const text = (html: string) => html.replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"').replace(/&amp;/g, '&').replace(/\s+/g, ' ');

test('the three choices are offered, with the folder one selected when it is on', () => {
  const html = render({ status: status() });
  for (const label of ['Off', 'A folder my cloud drive syncs', 'Another Grimoire (home server)']) assert.ok(text(html).includes(label), label);
  assert.match(html, /checked="" value="folder"/);
  assert.equal(initialMode(status()), 'folder');
  assert.equal(initialMode(status({ enabled: false, peer: 'http://home:9111' })), 'peer');
  assert.equal(initialMode(status({ enabled: false })), 'off');
});

test('status shows the folder, last sync, devices with names and last-seen times, and Sync now', () => {
  const t = text(render({ status: status() }));
  assert.ok(t.includes('~/Dropbox/Grimoire'));
  assert.ok(t.includes('Last synced 2 min ago · 3 devices'));
  assert.ok(t.includes('Laptop (this device)'));
  assert.ok(t.includes('Desktop · last seen 3 h ago'));
  assert.ok(t.includes('Old Mac · last seen 5 days ago · still downloading'));
  assert.ok(t.includes('Sync now'));
  assert.ok(t.includes('Set up on another device'));
  assert.ok(t.includes('Install Grimoire') && t.includes('Enter the same passphrase'));
});

test('errors are shown in plain words', () => {
  const wrong = status({ error: { code: 'wrong_passphrase', message: 'That passphrase does not match the one this backup was created with.' } });
  assert.ok(text(render({ status: wrong })).includes('That passphrase does not match'));
  assert.equal(problemText(status({ stats: { ...status().stats, waiting: 3 } })), 'Waiting for your cloud drive to finish downloading 3 files. Grimoire will try again shortly.');
  assert.equal(problemText(status()), null);
  assert.equal(deviceIssueText('starting'), 'setting up');
});

test('first setup asks for the passphrase twice and warns that it cannot be recovered', () => {
  const html = render({ status: status({ enabled: false, suggestions: [{ provider: 'dropbox', label: 'Dropbox', root: '/home/jo/Dropbox', path: '/home/jo/Dropbox/Grimoire' }] }),
    form: { folder: '/home/jo/Dropbox/Grimoire', passphrase: '', confirm: '', deviceName: 'Laptop' },
    probe: { path: '/home/jo/Dropbox/Grimoire', exists: false, has_backup: false, downloading: false, icloud: false } });
  assert.match(html, /id="sync-pass"/);
  assert.match(html, /id="sync-pass2"/);
  assert.ok(text(html).includes(FORGET_WARNING));
  assert.ok(text(html).includes('Dropbox'), 'detected drive offered');
  assert.ok(text(html).includes('Choose…'));
  assert.ok(text(html).includes('Start syncing'));
  assert.match(html, /id="sync-start"[^>]*disabled/);
});

test('joining an existing backup asks once', () => {
  const html = render({ status: status({ enabled: false }), form: { folder: '/x', passphrase: 'p', confirm: '', deviceName: '' },
    probe: { path: '/x', exists: true, has_backup: true, downloading: false, icloud: false } });
  assert.doesNotMatch(html, /id="sync-pass2"/);
  assert.ok(text(html).includes('already has a Grimoire backup'));
  assert.ok(text(html).includes('Connect this device'));
});

test('iCloud folders carry the Keep Downloaded hint', () => {
  const folder = '/Users/jo/Library/Mobile Documents/com~apple~CloudDocs/Grimoire';
  assert.ok(isICloudPath(folder));
  assert.ok(isICloudPath('C:\\Users\\jo\\iCloudDrive\\Grimoire'));
  assert.ok(!isICloudPath('/home/jo/Dropbox/Grimoire'));
  assert.ok(text(render({ status: status({ enabled: false }), form: { folder, passphrase: '', confirm: '', deviceName: '' } })).includes('Keep Downloaded'));
});

test('setup validation explains what is missing', () => {
  const fresh = { path: '/f', exists: true, has_backup: false, downloading: false, icloud: false };
  const form = { folder: '/f', passphrase: 'short', confirm: 'short', deviceName: '' };
  assert.equal(setupProblem({ ...form, folder: '' }, fresh), 'Choose a folder.');
  assert.equal(setupProblem(form, fresh), 'Use a passphrase of at least 8 characters.');
  assert.equal(setupProblem({ ...form, passphrase: 'long enough', confirm: 'long enougH' }, fresh), 'The two passphrases do not match.');
  assert.equal(setupProblem({ ...form, passphrase: 'long enough', confirm: 'long enough' }, fresh), null);
  assert.equal(setupProblem({ ...form, confirm: '' }, { ...fresh, has_backup: true }), null, 'an existing backup takes its passphrase as-is');
  assert.match(setupProblem(form, { ...fresh, downloading: true }) ?? '', /still downloading/);
  assert.ok(createsBackup(undefined));
});

test('turning it off is offered from the other choices while it is on', () => {
  const t = text(render({ status: status(), mode: 'off' }));
  assert.ok(t.includes('Folder sync is still on'));
  assert.ok(t.includes('Turn off folder sync'));
  assert.ok(text(render({ status: status({ enabled: false }), mode: 'peer' })).includes('GRIMOIRE_SYNC_PEER'));
});

test('times and paths read naturally', () => {
  assert.equal(relativeTime(0, NOW), 'never');
  assert.equal(relativeTime(NOW - 10_000, NOW), 'just now');
  assert.equal(relativeTime(NOW - 86400_000, NOW), 'yesterday');
  assert.equal(statusSummary(status({ devices: [status().devices[0]!] }), NOW), 'Last synced 2 min ago · 1 device');
  assert.equal(tildify('/home/jo/Dropbox', '/home/jo'), '~/Dropbox');
  assert.equal(tildify('/home/joe/x', '/home/jo'), '/home/joe/x');
});

test('restorable deleted notes are listed with a restore action', () => {
  const t = text(render({ status: status(), deleted: [{ path: 'ideas/old.md', deleted_at: NOW - 3600_000, device: 'Desktop', size: 10 }] }));
  assert.ok(t.includes('ideas/old.md deleted 1 h ago on Desktop'));
  assert.ok(t.includes('Restore'));
});
