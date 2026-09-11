import { useEffect, useMemo, useRef, useState } from 'react';

export type NewNoteInput = { title: string; folder: string; body: string };

function validFolder(folder: string) {
  return !folder || folder.split('/').every(segment => /^[A-Za-z0-9][A-Za-z0-9 _-]*$/.test(segment));
}

export function NewNotePanel({ folders, create, close }: { folders: string[]; create: (input: NewNoteInput) => Promise<void>; close: () => void }) {
  const [title, setTitle] = useState(''); const [folder, setFolder] = useState(''); const [body, setBody] = useState('');
  const [error, setError] = useState(''); const [busy, setBusy] = useState(false); const titleRef = useRef<HTMLInputElement>(null);
  const folderOptions = useMemo(() => [...new Set(folders)].sort(), [folders]);
  useEffect(() => { titleRef.current?.focus(); return () => document.getElementById('new-note')?.focus(); }, []);
  useEffect(() => { const update = () => { const viewport = window.visualViewport; document.documentElement.style.setProperty('--visible-height', `${viewport?.height || innerHeight}px`); document.documentElement.style.setProperty('--visible-top', `${viewport?.offsetTop || 0}px`); }; update(); addEventListener('resize', update); window.visualViewport?.addEventListener('resize', update); window.visualViewport?.addEventListener('scroll', update); return () => { removeEventListener('resize', update); window.visualViewport?.removeEventListener('resize', update); window.visualViewport?.removeEventListener('scroll', update); document.documentElement.style.removeProperty('--visible-height'); document.documentElement.style.removeProperty('--visible-top'); }; }, []);
  const submit = async (event: React.FormEvent) => {
    event.preventDefault(); const clean = folder.trim().replace(/^\/+|\/+$/g, '');
    if (!title.trim()) { setError('Give the note a title.'); titleRef.current?.focus(); return; }
    if (!validFolder(clean)) { setError('Use a folder name made of words, spaces, dashes, underscores, and nested folders.'); return; }
    setBusy(true); setError('');
    try { await create({ title: title.trim(), folder: clean, body }); } catch (reason) { setError(reason instanceof Error ? reason.message : 'Could not create note.'); setBusy(false); }
  };
  return <div id="new-note-modal" className="modal" role="dialog" aria-modal="true" aria-labelledby="new-note-heading" onMouseDown={event => event.currentTarget === event.target && close()}><form className="modal-box" onSubmit={event => void submit(event)} onKeyDown={event => { if (event.key === 'Escape' && !busy) { event.preventDefault(); event.stopPropagation(); close(); } }}><h2 id="new-note-heading">New note</h2><div className="new-note-fields"><label htmlFor="new-note-title">Title<input ref={titleRef} id="new-note-title" value={title} required autoComplete="off" onChange={event => setTitle(event.target.value)} placeholder="Give your note a name" /></label><label htmlFor="new-note-folder">Folder <span className="muted">optional</span><input id="new-note-folder" value={folder} list="new-note-folders" autoComplete="off" onChange={event => setFolder(event.target.value)} placeholder="e.g. Projects/Ideas" /></label><datalist id="new-note-folders">{folderOptions.map(option => <option value={option} key={option} />)}</datalist><label htmlFor="new-note-body">Start writing <span className="muted">optional</span><textarea id="new-note-body" rows={4} value={body} onChange={event => setBody(event.target.value)} placeholder="Capture an idea, paste something, or leave this blank…" /></label>{error && <p id="new-note-error" role="alert">{error}</p>}</div><footer className="new-note-actions"><button id="new-note-create" type="submit" className="btn primary" disabled={busy}>{busy ? 'Creating…' : 'Create note'}</button><button id="new-note-close" type="button" className="btn" disabled={busy} onClick={close}>Cancel</button><button id="new-note-cancel" type="button" className="btn" disabled={busy} onClick={close}>Cancel</button></footer></form></div>;
}
