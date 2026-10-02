// Pure logic for Settings → Sync & backup, kept apart from the component so it
// can be tested without a browser.

export type SyncMode = 'off' | 'folder' | 'peer';

export interface SyncError { code: string; message: string; detail?: string }
export interface SyncStats { uploaded: number; downloaded: number; deleted: number; merged: number; conflicts: number; waiting: number; failed: number; devices: number }
export interface SyncDevice { id: string; name: string; last_seen: number; self: boolean; issue?: string }
export interface SyncSuggestion { provider: string; label: string; root: string; path: string }
export interface FolderSyncStatus {
  enabled: boolean; folder: string; device_id: string; device_name: string; interval: number;
  running: boolean; last_sync: number; last_attempt: number; error?: SyncError | null;
  stats: SyncStats; devices: SyncDevice[]; icloud: boolean;
  suggestions: SyncSuggestion[]; peer: string | null; home: string;
}
export interface FolderProbe { path: string; exists: boolean; has_backup: boolean; downloading: boolean; icloud: boolean; problem?: SyncError | null }
export interface FolderListing { path: string; parent: string; dirs: string[] }
export interface DeletedNote { path: string; deleted_at: number; device: string; size: number }
export interface SetupForm { folder: string; passphrase: string; confirm: string; deviceName: string }

export const MIN_PASSPHRASE = 8;
export const FORGET_WARNING = "If you forget this, the backup can't be read. Grimoire can't recover it.";
export const ICLOUD_HINT = 'iCloud Drive can remove downloaded files to save space. In Finder, right-click this folder and choose "Keep Downloaded" so Grimoire can always read it.';

export function initialMode(status: Pick<FolderSyncStatus, 'enabled' | 'peer'> | undefined): SyncMode {
  if (!status) return 'off';
  if (status.enabled) return 'folder';
  return status.peer ? 'peer' : 'off';
}

export function relativeTime(ms: number, now = Date.now()): string {
  if (!ms) return 'never';
  const seconds = Math.max(0, Math.round((now - ms) / 1000));
  if (seconds < 45) return 'just now';
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} h ago`;
  const days = Math.round(hours / 24);
  return days === 1 ? 'yesterday' : `${days} days ago`;
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** "Last synced 2 min ago · 3 devices", the line under the folder path. */
export function statusSummary(status: FolderSyncStatus, now = Date.now()): string {
  if (status.running && !status.last_sync) return 'Syncing for the first time…';
  return `Last synced ${relativeTime(status.last_sync, now)} · ${plural(Math.max(1, status.devices.length), 'device')}`;
}

/** A plain sentence for what is wrong, or null when nothing is. */
export function problemText(status: FolderSyncStatus): string | null {
  if (status.error) return status.error.message || status.error.detail || 'Sync failed.';
  if (status.stats?.waiting > 0) return `Waiting for your cloud drive to finish downloading ${plural(status.stats.waiting, 'file')}. Grimoire will try again shortly.`;
  if (status.stats?.failed > 0) return `${plural(status.stats.failed, 'file')} from another device could not be saved here. Grimoire will keep trying.`;
  return null;
}

export function deviceIssueText(issue?: string): string {
  switch (issue) {
    case undefined: case '': return '';
    case 'not_downloaded': return 'still downloading';
    case 'starting': return 'setting up';
    default: return issue.replace(/_/g, ' ');
  }
}

/** Whether setting up this folder will create a new backup, which needs the passphrase twice. */
export function createsBackup(probe: FolderProbe | undefined): boolean {
  return !probe || !probe.has_backup;
}

/** What stops the form from being submitted, in words, or null. */
export function setupProblem(form: SetupForm, probe: FolderProbe | undefined): string | null {
  if (!form.folder.trim()) return 'Choose a folder.';
  if (probe?.problem) return probe.problem.message;
  if (probe?.downloading) return 'Your cloud drive is still downloading this folder. Wait until it finishes, then try again.';
  if (!form.passphrase) return 'Enter the passphrase.';
  if (createsBackup(probe)) {
    if ([...form.passphrase].length < MIN_PASSPHRASE) return `Use a passphrase of at least ${MIN_PASSPHRASE} characters.`;
    if (form.confirm !== form.passphrase) return 'The two passphrases do not match.';
  }
  return null;
}

export function isICloudPath(path: string): boolean {
  return /Mobile Documents\/com~apple~CloudDocs/.test(path) || /[\\/]iCloudDrive([\\/]|$)/i.test(path);
}

/** Show a path with the home directory as ~, which is how people recognise it. */
export function tildify(path: string, home: string): string {
  if (home && (path === home || path.startsWith(home + '/') || path.startsWith(home + '\\'))) return '~' + path.slice(home.length);
  return path;
}

export function joinPath(dir: string, name: string): string {
  const sep = dir.includes('\\') && !dir.includes('/') ? '\\' : '/';
  return dir.endsWith(sep) ? dir + name : dir + sep + name;
}
