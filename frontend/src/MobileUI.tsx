import type { createGrimoireApi } from './api';
import { useEffect, useRef, useState } from 'react';

type Actions = {
  visible: boolean; refresh: () => Promise<unknown> | void; request: ReturnType<typeof createGrimoireApi>['request'];
  ask: () => void; newNote: () => void; daily: () => void; evidence: () => void; banks: () => void; vault: () => void;
  graph: () => void; graphWarm: () => void; palette: () => void; settings: () => void; theme: () => void; openPath: (path: string) => void;
};
type Sheet = 'capture' | 'more' | null;

const icon = (d: string) => <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={d} /></svg>;
const ICONS = {
  notes: icon('M6 3h9l4 4v14H6zM14 3v5h5M9 13h7M9 17h7'),
  search: icon('M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM21 21l-4.5-4.5'),
  plus: icon('M12 5v14M5 12h14'),
  ask: icon('M12 3l2.2 5.8L20 11l-5.8 2.2L12 19l-2.2-5.8L4 11l5.8-2.2z'),
  more: icon('M5 12h.01M12 12h.01M19 12h.01'),
};

export function MobileChrome(a: Actions) {
  const [sheet, setSheet] = useState<Sheet>(null);
  const [msg, setMsg] = useState('');
  const toast = (m: string) => { setMsg(m); setTimeout(() => setMsg(''), 2200); };
  const [prefill, setPrefill] = useState<{ text: string; url: string; title: string }>();
  // Share target: the manifest sends shared text/links to /?title=&text=&url=.
  useEffect(() => {
    const q = new URLSearchParams(location.search);
    const text = q.get('text') || '', url = q.get('url') || '', title = q.get('title') || '';
    if (text || url || title) { setPrefill({ text, url: url || (/^https?:\/\/\S+$/.test(text) ? text : ''), title }); setSheet('capture'); history.replaceState(history.state, '', location.pathname + location.hash); }
  }, []);
  usePullToRefresh(a.refresh);
  useSheetSwipe();
  const close = () => setSheet(null);
  const pick = (fn: () => void) => () => { close(); fn(); };
  const focusSearch = () => { document.querySelector<HTMLInputElement>('#search')?.focus(); document.querySelector('#note-list')?.scrollTo({ top: 0 }); };
  return <>
    {a.visible && <nav id="tabbar" aria-label="Primary">
      <button className="tab on" aria-current="page" onClick={() => document.querySelector('#note-list')?.scrollTo({ top: 0, behavior: 'smooth' })}>{ICONS.notes}<span>Notes</span></button>
      <button className="tab" onClick={focusSearch}>{ICONS.search}<span>Search</span></button>
      <button className="tab fab" aria-label="Capture" onClick={() => setSheet('capture')}>{ICONS.plus}</button>
      <button className="tab" onClick={a.ask}>{ICONS.ask}<span>Ask</span></button>
      <button className="tab" aria-haspopup="dialog" onClick={() => setSheet('more')}>{ICONS.more}<span>More</span></button>
    </nav>}
    <div id="ptr" aria-hidden="true"><span /></div>
    {sheet === 'more' && <div className="modal sheet" id="more-sheet" role="dialog" aria-label="More" onMouseDown={e => e.currentTarget === e.target && close()}><div className="modal-box">
      <div className="sheet-grid">
        <button onClick={pick(a.daily)}><b>◈</b>Today</button>
        <button onClick={pick(a.evidence)}><b>❝</b>Evidence</button>
        <button onClick={pick(a.banks)}><b>▤</b>Banks</button>
        <button onClick={pick(a.vault)}><b>◇</b>Vault</button>
        <button onPointerDown={a.graphWarm} onClick={pick(a.graph)}><b>◉</b>Graph</button>
        <button onClick={pick(a.palette)}><b>⌘</b>Commands</button>
        <button onClick={pick(a.settings)}><b>⚙</b>Settings</button>
        <button onClick={a.theme}><b>◐</b>Theme</button>
      </div>
    </div></div>}
    {sheet === 'capture' && <CaptureSheet close={close} prefill={prefill} request={a.request} newNote={pick(a.newNote)} refresh={a.refresh} toast={toast} />}
    {msg && <div className="m-toast" role="status">{msg}</div>}
  </>;
}

function CaptureSheet({ close, prefill, request, newNote, refresh, toast }: { close: () => void; prefill?: { text: string; url: string; title: string }; request: Actions['request']; newNote: () => void; refresh: Actions['refresh']; toast: (m: string) => void }) {
  const [text, setText] = useState(prefill?.text && prefill.text !== prefill.url ? prefill.text : '');
  const [url] = useState(prefill?.url || ''); const [busy, setBusy] = useState(false); const [error, setError] = useState('');
  const ref = useRef<HTMLTextAreaElement>(null);
  useEffect(() => { ref.current?.focus(); }, []);
  const save = async () => {
    if (!text.trim() && !url) return; setBusy(true); setError('');
    try { await request('/capture', { method: 'POST', body: { text: text.trim(), url, title: prefill?.title || '' } }); toast('Captured to inbox'); void refresh(); close(); }
    catch (e) { setError(e instanceof Error ? e.message : 'Could not save'); setBusy(false); }
  };
  return <div className="modal sheet" id="capture-sheet" role="dialog" aria-label="Capture" onMouseDown={e => e.currentTarget === e.target && close()}><div className="modal-box">
    <h2 className="sheet-title">Quick capture</h2>
    {url && <p className="capture-url">{url}</p>}
    <textarea ref={ref} id="capture-text" aria-label="Capture text" value={text} onChange={e => setText(e.target.value)} placeholder="A thought, a link, anything…" rows={5} />
    {error && <p role="alert" className="capture-err">{error}</p>}
    <div className="sheet-actions"><button className="btn" onClick={newNote}>New note…</button><button id="capture-save" className="btn primary" disabled={busy || (!text.trim() && !url)} onClick={() => void save()}>{busy ? 'Saving…' : 'Save'}</button></div>
  </div></div>;
}

// Pull the notes list down from its top to refresh.
function usePullToRefresh(refresh: Actions['refresh']) {
  const fn = useRef(refresh); fn.current = refresh;
  useEffect(() => {
    let y0 = -1, pull = 0, busy = false;
    const bar = () => document.getElementById('ptr');
    const set = (px: number, spin = false) => { const b = bar(); if (!b) return; b.style.setProperty('--p', String(px / 64)); b.classList.toggle('spin', spin); b.classList.toggle('ready', px >= 64); };
    const list = () => document.getElementById('note-list');
    const start = (e: TouchEvent) => { const l = list(); y0 = innerWidth <= 780 && !busy && l && l.contains(e.target as Node) && l.scrollTop <= 0 && e.touches.length === 1 ? e.touches[0]!.clientY : -1; pull = 0; };
    const move = (e: TouchEvent) => { if (y0 < 0) return; const dy = e.touches[0]!.clientY - y0; if (dy <= 0) { if (pull) set(0); pull = 0; return; } pull = Math.min(96, dy * 0.5); set(pull); };
    const end = async () => { if (y0 < 0) return; const go = pull >= 64; y0 = -1; if (!go) { set(0); return; } busy = true; set(48, true); try { await fn.current(); } catch { /* shown by caller */ } busy = false; set(0); };
    document.addEventListener('touchstart', start, { passive: true }); document.addEventListener('touchmove', move, { passive: true }); document.addEventListener('touchend', end, { passive: true });
    return () => { document.removeEventListener('touchstart', start); document.removeEventListener('touchmove', move); document.removeEventListener('touchend', end); };
  }, []);
}

// Drag a bottom sheet down by its top edge to dismiss it.
function useSheetSwipe() {
  useEffect(() => {
    let box: HTMLElement | null = null, y0 = 0, dy = 0;
    const start = (e: TouchEvent) => {
      box = null; if (innerWidth > 780) return;
      const b = (e.target as Element).closest<HTMLElement>('.modal:not(#graph-modal):not(#banks-modal):not(#memuse-modal):not(#knowledge-modal):not(#canvas-view):not(#vault-modal):not(#ask-modal):not(#new-note-modal):not(#palette) > .modal-box'); if (!b) return;
      const t = e.touches[0]!; if (t.clientY - b.getBoundingClientRect().top > 44) return;
      box = b; y0 = t.clientY; dy = 0; b.style.transition = 'none';
    };
    const move = (e: TouchEvent) => { if (!box) return; dy = Math.max(0, e.touches[0]!.clientY - y0); box.style.transform = `translateY(${dy}px)`; };
    const end = () => { if (!box) return; const b = box; box = null; b.style.transition = ''; if (dy > 90) b.parentElement?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true })); else b.style.transform = ''; };
    document.addEventListener('touchstart', start, { passive: true }); document.addEventListener('touchmove', move, { passive: true }); document.addEventListener('touchend', end, { passive: true });
    return () => { document.removeEventListener('touchstart', start); document.removeEventListener('touchmove', move); document.removeEventListener('touchend', end); };
  }, []);
}
