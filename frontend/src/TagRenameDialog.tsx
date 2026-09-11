import {useEffect, useRef, useState} from 'react';

export function TagRenameDialog({rename, close}: {rename: (oldTag: string, newTag: string) => Promise<void>; close: () => void}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [oldTag, setOldTag] = useState('');
  const [newTag, setNewTag] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => { dialog.current?.showModal(); }, []);
  return <dialog ref={dialog} className="react-note-dialog" onCancel={event => {event.preventDefault(); if (!busy) close();}}>
    <form className="form-panel" onKeyDown={event => event.stopPropagation()} onSubmit={event => {
      event.preventDefault(); if (busy) return;
      setBusy(true); setError('');
      void rename(oldTag.trim().replace(/^#/, ''), newTag.trim().replace(/^#/, ''))
        .then(close).catch(reason => setError(reason instanceof Error ? reason.message : String(reason)))
        .finally(() => setBusy(false));
    }}>
      <h2>Rename a tag across all notes</h2>
      <label>Current tag<input name="old" autoFocus required value={oldTag} disabled={busy} onChange={event => setOldTag(event.target.value)}/></label>
      <label>New tag<input name="new" required value={newTag} disabled={busy} onChange={event => setNewTag(event.target.value)}/></label>
      {error && <p role="alert">{error}</p>}
      <button type="submit" disabled={busy || !oldTag.trim() || !newTag.trim()}>{busy ? 'Renaming…' : 'Rename tag'}</button>
      <button type="button" disabled={busy} onClick={close}>Cancel</button>
    </form>
  </dialog>;
}
