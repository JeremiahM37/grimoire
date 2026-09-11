import { useEffect, useState } from 'react';
import type { TagCount } from './types';

export function TagBrowser({ load, close, choose, onError }: {
  load: () => Promise<TagCount[]>;
  close: () => void;
  choose: (tag: string) => void;
  onError: (message: string) => void;
}) {
  const [tags, setTags] = useState<TagCount[]>();
  useEffect(() => { let live = true; void load().then(rows => { if (live) setTags(rows); }).catch(e => onError(e instanceof Error ? e.message : 'Could not load tags')); return () => { live = false; }; }, [load, onError]);
  return <div id="tags-browser-modal" className="modal" role="dialog" onMouseDown={e => e.currentTarget === e.target && close()}><div className="modal-box"><header className="modal-head"><span>🏷 Tags <span id="tags-browser-count" className="graph-stat">{tags?.length || 0}</span></span><button id="tags-browser-close" className="icon" onClick={close}>✕</button></header><div id="tags-browser-body">{!tags ? <p className="vault-note">Loading tags…</p> : !tags.length ? <p className="vault-note">No tags yet. Add <code>#tags</code> to any note.</p> : <div className="tag-cloud">{tags.map(row => <button className="tag-chip" key={row.tag} onClick={() => { choose(row.tag); close(); }}>#{row.tag} <span>{row.c}</span></button>)}</div>}</div></div></div>;
}
