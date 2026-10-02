import { useCallback, useEffect, useRef, useState } from 'react';
import type { JsonValue } from './types';
import {
  FORGET_WARNING, ICLOUD_HINT, createsBackup, deviceIssueText, initialMode, isICloudPath, joinPath,
  problemText, relativeTime, setupProblem, statusSummary, tildify,
  type DeletedNote, type FolderListing, type FolderProbe, type FolderSyncStatus, type SetupForm, type SyncMode,
} from './syncSettings';

type Request = <T>(path: string, init?: { method?: string; body?: JsonValue }) => Promise<T>;

export interface SyncSettingsViewProps {
  status?: FolderSyncStatus;
  mode: SyncMode;
  form: SetupForm;
  probe?: FolderProbe;
  browse?: FolderListing;
  deleted?: DeletedNote[];
  busy?: string;
  notice?: string;
  error?: string;
  now?: number;
  onMode: (mode: SyncMode) => void;
  onForm: (form: SetupForm) => void;
  onSetup: () => void;
  onSyncNow: () => void;
  onPeerSyncNow: () => void;
  onTurnOff: () => void;
  onBrowse: (path: string) => void;
  onCloseBrowse: () => void;
  onLoadDeleted: () => void;
  onRestore: (path: string) => void;
}

/** Settings → Sync & backup. Presentational: everything it shows comes in as props. */
export function SyncSettingsView(p: SyncSettingsViewProps) {
  const { status, mode, form, probe } = p;
  const home = status?.home ?? '';
  const creating = createsBackup(probe);
  const blocked = setupProblem(form, probe);
  const icloud = isICloudPath(form.folder) || (mode === 'folder' && status?.enabled && status.icloud);
  const problem = status?.enabled ? problemText(status) : null;
  const radio = (value: SyncMode, label: string) => (
    <label className="sync-choice">
      <input type="radio" name="sync-mode" value={value} checked={mode === value} onChange={() => p.onMode(value)} /> {label}
    </label>
  );
  return (
    <section id="set-sync" aria-labelledby="set-sync-title">
      <h3 id="set-sync-title">Sync &amp; backup</h3>
      <div role="radiogroup" aria-label="Sync & backup">
        {radio('off', 'Off')}
        {radio('folder', 'A folder my cloud drive syncs')}
        {mode === 'folder' && (status?.enabled ? (
          <div className="sync-detail" id="sync-status">
            <p className="sync-folder"><code>{tildify(status.folder, home)}</code></p>
            <p className="sync-line">{statusSummary(status, p.now)}</p>
            {problem && <p className="sync-problem" role="alert">{problem}</p>}
            {status.icloud && <p className="vault-note">{ICLOUD_HINT}</p>}
            <ul className="sync-devices" aria-label="Devices">
              {status.devices.map(d => (
                <li key={d.id}>
                  <b>{d.name || 'Unnamed device'}</b>{d.self ? ' (this device)' : ''}
                  <small> · last seen {relativeTime(d.last_seen, p.now)}{d.issue ? ` · ${deviceIssueText(d.issue)}` : ''}</small>
                </li>
              ))}
            </ul>
            <div className="sync-actions">
              <button id="sync-now" className="btn" disabled={!!p.busy || status.running} onClick={p.onSyncNow}>{p.busy === 'sync' || status.running ? 'Syncing…' : 'Sync now'}</button>
              <button className="btn" onClick={p.onLoadDeleted}>Deleted notes…</button>
            </div>
            {p.deleted && (
              <div className="sync-deleted">
                {p.deleted.length ? p.deleted.map(d => (
                  <div className="set-row" key={d.path}>
                    <span title={d.path}>{d.path} <small>deleted {relativeTime(d.deleted_at, p.now)} on {d.device}</small></span>
                    <button className="btn" onClick={() => p.onRestore(d.path)}>Restore</button>
                  </div>
                )) : <p className="vault-note">Nothing deleted in the last 90 days.</p>}
              </div>
            )}
            <details className="sync-help">
              <summary>Set up on another device</summary>
              <ol>
                <li>Install Grimoire on the other computer.</li>
                <li>Make sure your cloud drive has finished syncing this folder there.</li>
                <li>In Settings, Sync &amp; backup, choose the same folder: <code>{tildify(status.folder, home)}</code></li>
                <li>Enter the same passphrase.</li>
              </ol>
              <p className="vault-note">That device then downloads every note. Keep using each device as normal; changes reach the others within a minute or two of your cloud drive syncing them.</p>
            </details>
          </div>
        ) : (
          <div className="sync-detail" id="sync-setup">
            <p className="vault-note">Pick a folder that Dropbox, iCloud Drive, OneDrive or Google Drive already syncs. Grimoire keeps an encrypted copy of your notes there and keeps your devices in step through it. Nothing new to sign up for.</p>
            {!!status?.suggestions.length && (
              <div className="sync-suggestions" aria-label="Detected cloud folders">
                {status.suggestions.map(s => (
                  <button key={s.path} type="button" className={'chip' + (form.folder === s.path ? ' on' : '')} onClick={() => p.onForm({ ...form, folder: s.path })}>{s.label}</button>
                ))}
              </div>
            )}
            <label className="set-row"><span>Folder</span>
              <input id="sync-folder" value={form.folder} placeholder="~/Dropbox/Grimoire" onChange={e => p.onForm({ ...form, folder: e.target.value })} />
              <button type="button" className="btn sync-choose" onClick={() => p.onBrowse(form.folder)}>Choose…</button>
            </label>
            {p.browse && (
              <div className="sync-browse" role="dialog" aria-label="Choose a folder">
                <p className="sync-folder"><code>{tildify(p.browse.path, home)}</code></p>
                <div className="sync-browse-list">
                  {p.browse.parent && <button type="button" onClick={() => p.onBrowse(p.browse!.parent)}>↑ Up</button>}
                  {p.browse.dirs.map(d => <button type="button" key={d} onClick={() => p.onBrowse(joinPath(p.browse!.path, d))}>{d}</button>)}
                </div>
                <div className="sync-actions">
                  <button type="button" className="btn" onClick={() => { p.onForm({ ...form, folder: p.browse!.path }); p.onCloseBrowse(); }}>Use this folder</button>
                  <button type="button" className="btn" onClick={() => { p.onForm({ ...form, folder: joinPath(p.browse!.path, 'Grimoire') }); p.onCloseBrowse(); }}>New "Grimoire" folder here</button>
                  <button type="button" className="btn" onClick={p.onCloseBrowse}>Cancel</button>
                </div>
              </div>
            )}
            {probe?.has_backup && <p className="vault-note sync-found">This folder already has a Grimoire backup. Enter its passphrase to connect this device.</p>}
            {probe && !probe.has_backup && !probe.downloading && !probe.problem && form.folder.trim() && <p className="vault-note">No backup here yet. A new one will be created.</p>}
            {icloud && <p className="vault-note">{ICLOUD_HINT}</p>}
            <label className="set-row"><span>Encryption passphrase</span>
              <input id="sync-pass" type="password" autoComplete={creating ? 'new-password' : 'current-password'} value={form.passphrase} onChange={e => p.onForm({ ...form, passphrase: e.target.value })} />
            </label>
            {creating && (
              <label className="set-row"><span>Confirm passphrase</span>
                <input id="sync-pass2" type="password" autoComplete="new-password" value={form.confirm} onChange={e => p.onForm({ ...form, confirm: e.target.value })} />
              </label>
            )}
            <p className="sync-warning">{FORGET_WARNING}</p>
            <label className="set-row"><span>This device's name</span>
              <input id="sync-device" value={form.deviceName} placeholder="Laptop" onChange={e => p.onForm({ ...form, deviceName: e.target.value })} />
            </label>
            {form.folder.trim() && form.passphrase && blocked && <p className="vault-note">{blocked}</p>}
            <button id="sync-start" className="btn" disabled={!!blocked || !!p.busy} onClick={p.onSetup}>{p.busy === 'setup' ? 'Setting up…' : creating ? 'Start syncing' : 'Connect this device'}</button>
          </div>
        ))}
        {radio('peer', 'Another Grimoire (home server)')}
        {mode === 'peer' && (
          <div className="sync-detail">
            {status?.peer ? (
              <>
                <p className="vault-note">Syncing with <code>{status.peer}</code>.</p>
                <button className="btn" disabled={!!p.busy} onClick={p.onPeerSyncNow}>{p.busy === 'peer' ? 'Syncing…' : 'Sync now'}</button>
              </>
            ) : (
              <p className="vault-note">Set <code>GRIMOIRE_SYNC_PEER</code> (and <code>GRIMOIRE_SYNC_TOKEN</code>) to your home server's address and restart Grimoire. See the README.</p>
            )}
          </div>
        )}
        {mode !== 'folder' && status?.enabled && (
          <div className="sync-detail">
            <p className="vault-note">Folder sync is still on, through <code>{tildify(status.folder, home)}</code>.</p>
            <button id="sync-off" className="btn" disabled={!!p.busy} onClick={p.onTurnOff}>Turn off folder sync</button>
          </div>
        )}
      </div>
      {p.notice && <p className="vault-note" role="status">{p.notice}</p>}
      {p.error && <p className="sync-problem" role="alert">{p.error}</p>}
    </section>
  );
}

const emptyForm: SetupForm = { folder: '', passphrase: '', confirm: '', deviceName: '' };

/** The live section: loads status, probes the chosen folder, and runs the actions. */
export function SyncSettings({ request }: { request: Request }) {
  const [status, setStatus] = useState<FolderSyncStatus>();
  const [mode, setMode] = useState<SyncMode>('off');
  const [form, setForm] = useState<SetupForm>(emptyForm);
  const [probe, setProbe] = useState<FolderProbe>();
  const [browse, setBrowse] = useState<FolderListing>();
  const [deleted, setDeleted] = useState<DeletedNote[]>();
  const [busy, setBusy] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [error, setError] = useState<string>();
  const [now, setNow] = useState(Date.now());
  const modeChosen = useRef(false);

  const load = useCallback(() => request<FolderSyncStatus>('/sync/folder').then(s => {
    setStatus(s);
    if (!modeChosen.current) setMode(initialMode(s));
    setForm(f => ({ ...f, deviceName: f.deviceName || s.device_name, folder: f.folder || s.suggestions[0]?.path || '' }));
  }).catch((e: unknown) => setError(e instanceof Error ? e.message : String(e))), [request]);

  useEffect(() => { void load(); const t = setInterval(() => { setNow(Date.now()); void load(); }, 15000); return () => clearInterval(t); }, [load]);

  // Probe the folder as it is typed, so the form knows whether to ask twice.
  useEffect(() => {
    const folder = form.folder.trim();
    if (!folder || status?.enabled) { setProbe(undefined); return; }
    const t = setTimeout(() => {
      request<FolderProbe>(`/sync/folder/probe?path=${encodeURIComponent(folder)}`).then(setProbe).catch(() => setProbe(undefined));
    }, 300);
    return () => clearTimeout(t);
  }, [form.folder, status?.enabled, request]);

  const act = async (name: string, fn: () => Promise<void>) => {
    setBusy(name); setError(undefined); setNotice(undefined);
    try { await fn(); } catch (e) { setError(e instanceof Error ? e.message : String(e)); } finally { setBusy(undefined); }
  };

  return <SyncSettingsView
    status={status} mode={mode} form={form} probe={probe} browse={browse} deleted={deleted}
    busy={busy} notice={notice} error={error} now={now}
    onMode={m => { modeChosen.current = true; setMode(m); }}
    onForm={setForm}
    onSetup={() => void act('setup', async () => {
      const out = await request<{ created: boolean; status: FolderSyncStatus }>('/sync/folder', { method: 'POST', body: {
        folder: form.folder.trim(), passphrase: form.passphrase, create: createsBackup(probe), device_name: form.deviceName.trim() } });
      setStatus(out.status); setForm({ ...emptyForm, deviceName: form.deviceName });
      setNotice(out.created ? 'Started a new backup. This device is syncing.' : 'Connected. This device is downloading your notes.');
    })}
    onSyncNow={() => void act('sync', async () => {
      const out = await request<{ status: FolderSyncStatus }>('/sync/folder/now', { method: 'POST' });
      setStatus(out.status);
    })}
    onPeerSyncNow={() => void act('peer', async () => {
      const out = await request<{ pulled: number; pushed: number; conflicts: number }>('/sync/now', { method: 'POST' });
      setNotice(`Pulled ${out.pulled}, pushed ${out.pushed}${out.conflicts ? `, ${out.conflicts} conflicts kept as copies` : ''}.`);
    })}
    onTurnOff={() => void act('off', async () => {
      if (!confirm('Turn off folder sync on this device? The backup in the folder is kept, and your other devices keep syncing.')) return;
      setStatus(await request<FolderSyncStatus>('/sync/folder', { method: 'DELETE' }));
      setNotice('Folder sync is off on this device.');
    })}
    onBrowse={path => void act('browse', async () => {
      setBrowse(await request<FolderListing>(`/sync/folder/browse?path=${encodeURIComponent(path)}`));
    })}
    onCloseBrowse={() => setBrowse(undefined)}
    onLoadDeleted={() => void act('deleted', async () => {
      setDeleted((await request<{ deleted: DeletedNote[] }>('/sync/folder/deleted')).deleted);
    })}
    onRestore={path => void act('restore', async () => {
      await request('/sync/folder/restore', { method: 'POST', body: { path } });
      setDeleted(d => d?.filter(x => x.path !== path));
      setNotice(`Restored ${path}.`);
    })}
  />;
}
