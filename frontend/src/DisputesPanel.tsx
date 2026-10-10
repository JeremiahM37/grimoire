import { useCallback, useEffect, useMemo, useState } from 'react';
import { createDisputesApi, type DisputesApi, type Request } from './disputesApi';
import { authorityText, contestText, RESOLUTION_LABEL, resolveBody, type Dispute, type DisputeSide, type Resolution } from './disputesModel';

// Disputes: a fact a person recorded, and the newer fact an agent contradicted
// and was not allowed to replace. A person settles each one: keep theirs,
// accept the agent's, or write a merged fact. Settling is a person's act, so
// this panel is only reachable from the console, not from an agent.

function Side({ label, side }: { label: string; side: DisputeSide }) {
  return <div className="dispute-side">
    <div className="dispute-label">{label}</div>
    <p className="dispute-text">{side.text}</p>
    <div className="dispute-meta">{authorityText(side.authority)}{side.agent ? ` · ${side.agent}` : ''}{side.stamp ? ` · ${side.stamp}` : ''}{side.evidence.length ? ` · from ${side.evidence.join(', ')}` : ''}</div>
  </div>;
}

export function DisputesPanel({ request, close, openNote }: { request: Request; close: () => void; openNote?: (path: string) => void }) {
  const api: DisputesApi = useMemo(() => createDisputesApi(request), [request]);
  const [rows, setRows] = useState<Dispute[] | undefined>();
  const [error, setError] = useState<string>('');
  const [busy, setBusy] = useState(false);

  const reload = useCallback(async (signal?: AbortSignal) => {
    try { setRows(await api.list(signal)); setError(''); } catch (e) { if (!signal?.aborted) setError(e instanceof Error ? e.message : String(e)); }
  }, [api]);
  useEffect(() => { const c = new AbortController(); void reload(c.signal); return () => c.abort(); }, [reload]);

  return <div id="disputes-modal" className="modal" role="dialog" aria-label="Memory disputes" onMouseDown={e => e.currentTarget === e.target && close()}>
    <div className="modal-box banks-box">
      <button id="disputes-close" className="icon modal-close" aria-label="Close" onClick={close}>✕</button>
      <div className="banks-head"><h2 className="banks-name">Disputes</h2></div>
      <div className="banks-body" data-testid="disputes-view">
        <p className="vault-note">Facts a person recorded that an agent contradicts. Each stays as it is until you settle it.</p>
        {error && <p className="vault-note" role="alert">{error}</p>}
        {rows === undefined && !error && <p className="vault-note">Loading…</p>}
        {rows?.length === 0 && <p className="vault-note" data-testid="disputes-empty">No open disputes. Nothing an agent contests is waiting for you.</p>}
        {rows?.map(d => <DisputeCard key={d.id} dispute={d} busy={busy} openNote={openNote} setBusy={setBusy} setError={setError} reload={reload} api={api} />)}
      </div>
    </div>
  </div>;
}

function DisputeCard({ dispute: d, busy, openNote, setBusy, setError, reload, api }: {
  dispute: Dispute; busy: boolean; openNote?: (path: string) => void; setBusy: (b: boolean) => void; setError: (e: string) => void;
  reload: () => Promise<void>; api: DisputesApi;
}) {
  const [choice, setChoice] = useState<Resolution>('keep');
  const [challenger, setChallenger] = useState<string>(d.challengers[0]?.id ?? '');
  const [text, setText] = useState<string>(d.disputed.text);

  const settle = async () => {
    const built = resolveBody(d, choice, { text, challenger });
    if (!built.ok) { setError(built.error); return; }
    setBusy(true);
    try {
      await api.resolve(built.body);
      setError('');
      await reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return <section className="dispute-card" data-testid="dispute" aria-label={`Dispute in ${d.path}`}>
    <div className="dispute-head">
      <button className="link" onClick={() => openNote?.(d.path)} disabled={!openNote}>{d.path}</button>
      <span className="dispute-count">{contestText(d)}</span>
    </div>
    <Side label="Kept as it stands" side={d.disputed} />
    {d.challengers.map(c => <Side key={c.id} label="Contested by" side={c} />)}
    <fieldset className="dispute-choice" disabled={busy}>
      <legend>Settle it</legend>
      {(['keep', 'accept_challenger', 'merge'] as Resolution[]).map(r => <label key={r}>
        <input type="radio" name={`dispute-${d.id}`} checked={choice === r} onChange={() => setChoice(r)} /> {RESOLUTION_LABEL[r]}
      </label>)}
      {choice === 'accept_challenger' && d.challengers.length > 1 && <label>Which fact
        <select value={challenger} onChange={e => setChallenger(e.target.value)}>
          {d.challengers.map(c => <option key={c.id} value={c.id}>{c.text}</option>)}
        </select>
      </label>}
      {choice === 'merge' && <label>The fact as it should read
        <textarea value={text} onChange={e => setText(e.target.value)} rows={3} />
      </label>}
      <button className="btn primary" onClick={() => void settle()} disabled={busy}>Settle</button>
    </fieldset>
  </section>;
}
