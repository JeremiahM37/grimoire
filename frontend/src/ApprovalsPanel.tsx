import { useCallback, useEffect, useState } from 'react';
import { ApiError, type createGrimoireApi } from './api';
import {
  type ConnectorName, type SourceAction, clip, isDecided, isPending, kindLabel, outcomeText,
  pendingCount, paramLines, plainAction, safeLink, sortHistory, stateLabel, whenText,
} from './approvals';

type API = ReturnType<typeof createGrimoireApi>;
type Tab = 'pending' | 'history';

export const ADMIN_HINT = 'Approvals need the administrative credential. Open the console with its administrative credential; nothing here is shown or changed without it.';

// The admin gate refuses with 401 + X-Grimoire-Gate: admin, or 403 for a
// signed-in non-administrator. Both get the same plain explanation.
export function approvalsErrorText(error: unknown): string {
  if (error instanceof ApiError && (error.gate === 'admin' || error.status === 401 || error.status === 403)) return ADMIN_HINT;
  return error instanceof Error ? error.message : String(error);
}

// Pending count for the badges. Polls while the panel's owner can use it; a
// refused or failed poll reads as zero rather than as an error in the chrome.
export function usePendingApprovalCount(api: API, enabled: boolean): [number, () => void] {
  const [count, setCount] = useState(0);
  const tick = useCallback(() => {
    if (!enabled) return;
    api.request<SourceAction[]>('/source-actions?state=pending')
      .then(rows => setCount(pendingCount(rows)))
      .catch(() => setCount(0));
  }, [api, enabled]);
  useEffect(() => {
    if (!enabled) { setCount(0); return; }
    tick();
    const timer = setInterval(tick, 30000);
    addEventListener('focus', tick);
    return () => { clearInterval(timer); removeEventListener('focus', tick); };
  }, [enabled, tick]);
  return [count, tick];
}

// Whether the signed-in identity should see approvals at all. Non-admins would
// only collect 401s from the admin routes.
export const approvalsAllowed = (identity?: { admin: boolean; multi_user: boolean }) =>
  !!identity && (identity.admin || !identity.multi_user);

export function ApprovalsPanel({ api, close, onChanged }: { api: API; close: () => void; onChanged?: () => void }) {
  const [tab, setTab] = useState<Tab>('pending');
  const [actions, setActions] = useState<SourceAction[]>();
  const [connectors, setConnectors] = useState<ConnectorName[]>([]);
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState<string>();
  const [confirming, setConfirming] = useState<string>();
  const [open, setOpen] = useState<string>();
  const [latest, setLatest] = useState<SourceAction[]>([]);

  const load = useCallback(() => {
    setError(undefined);
    return Promise.all([
      api.request<SourceAction[]>('/source-actions'),
      api.request<ConnectorName[]>('/connectors').catch(() => [] as ConnectorName[]),
    ]).then(([rows, conns]) => { setActions(rows); setConnectors(conns); })
      .catch(reason => setError(reason));
  }, [api]);

  useEffect(() => { void load(); }, [load]);

  const decide = (action: SourceAction, approve: boolean) => {
    setBusy(action.id);
    setError(undefined);
    api.request<SourceAction>(`/source-actions/${encodeURIComponent(action.id)}/${approve ? 'approve' : 'deny'}`, { method: 'POST', body: {} })
      .then(record => {
        setLatest(prev => [record, ...prev.filter(r => r.id !== record.id)].slice(0, 5));
        setConfirming(undefined);
        setOpen(undefined);
        onChanged?.();
        return load();
      })
      .catch(reason => setError(reason))
      .finally(() => setBusy(undefined));
  };

  const connectorFor = (a: SourceAction) => connectors.find(c => c.id === a.source);
  const pending = (actions ?? []).filter(isPending);
  const history = sortHistory(actions ?? []);

  const card = (a: SourceAction) => {
    const connector = connectorFor(a);
    const headline = plainAction(a, connector);
    const expanded = open === a.id;
    const lines = paramLines(a.params);
    return (
      <article className="ap-card" key={a.id} data-testid="approval-card" data-id={a.id} data-state={a.state}>
        <header className="ap-head">
          <h3 className="ap-title">{headline}</h3>
          <span className="ap-kind">{connector?.name ? `${connector.name} · ` : ''}{kindLabel(a.kind || connector?.kind)}</span>
        </header>
        <p className="ap-meta">Asked by <b>{a.agent || 'an agent'}</b> · <time dateTime={a.created}>{whenText(a.created)}</time></p>
        <button className="ap-expand" aria-expanded={expanded} onClick={() => setOpen(expanded ? undefined : a.id)}>
          {expanded ? 'Hide full parameters' : 'Show full parameters'}
        </button>
        {expanded && <pre className="ap-params" aria-label="Full parameters">{lines.length ? lines.join('\n') : '(no parameters)'}</pre>}
        {confirming === a.id ? (
          <div className="ap-confirm" role="group" aria-label="Confirm approval">
            <p><b>Approve and run this now?</b> {clip(headline, 220)} It runs for real, using the parameters shown above.</p>
            <div className="ap-actions">
              <button className="ap-approve" disabled={busy === a.id} onClick={() => decide(a, true)}>{busy === a.id ? 'Running…' : 'Yes, approve'}</button>
              <button className="ap-cancel" disabled={busy === a.id} onClick={() => setConfirming(undefined)}>Cancel</button>
            </div>
          </div>
        ) : (
          <div className="ap-actions">
            <button className="ap-approve" disabled={!!busy} onClick={() => setConfirming(a.id)}>Approve…</button>
            <button className="ap-deny" disabled={!!busy} onClick={() => decide(a, false)}>Deny</button>
          </div>
        )}
      </article>
    );
  };

  const decidedCard = (a: SourceAction) => {
    const connector = connectorFor(a);
    const link = safeLink(a.result?.url);
    return (
      <article className="ap-card ap-decided" key={a.id} data-testid="history-card" data-id={a.id} data-state={a.state}>
        <header className="ap-head">
          <h3 className="ap-title">{plainAction(a, connector)}</h3>
          <span className={`ap-state ap-state-${a.state}`}>{stateLabel(a.state)}</span>
        </header>
        <p className="ap-outcome">{outcomeText(a)}</p>
        <p className="ap-meta">
          {a.agent ? <>Asked by <b>{a.agent}</b> · </> : null}
          {a.decided_by ? <>decided by <b>{a.decided_by}</b> · </> : null}
          <time dateTime={a.decided || a.created}>{whenText(a.decided || a.created)}</time>
        </p>
        {link && <p className="ap-meta"><a href={link} target="_blank" rel="noopener noreferrer">Open result</a></p>}
      </article>
    );
  };

  const hint = error ? approvalsErrorText(error) : '';

  return (
    <div id="approvals-modal" className="modal" role="dialog" aria-labelledby="approvals-title" onMouseDown={e => e.currentTarget === e.target && close()}>
      <div className="modal-box approvals-box">
        <button id="approvals-close" className="icon modal-close" aria-label="Close approvals" onClick={close}>✕</button>
        <h2 id="approvals-title">Approvals</h2>
        <div className="ap-tabs" role="tablist" aria-label="Approvals">
          <button role="tab" id="ap-tab-pending" aria-selected={tab === 'pending'} className={tab === 'pending' ? 'on' : ''} onClick={() => setTab('pending')}>
            Waiting{actions && pending.length > 0 ? <span className="ap-count">{pending.length}</span> : null}
          </button>
          <button role="tab" id="ap-tab-history" aria-selected={tab === 'history'} className={tab === 'history' ? 'on' : ''} onClick={() => setTab('history')}>History</button>
        </div>
        <div id="approvals-body" className="ap-body">
          {hint && <p className="ap-error" role="alert">{hint}</p>}
          {!actions && !error && <p className="ap-empty">Loading…</p>}
          {actions && tab === 'pending' && (
            <>
              {latest.length > 0 && (
                <section className="ap-latest" aria-label="Latest decisions">
                  {latest.map(r => (
                    <p key={r.id} className={`ap-result ap-result-${r.state}`} role="status">
                      <b>{stateLabel(r.state)}.</b> {clip(plainAction(r, connectorFor(r)), 90)} {outcomeText(r)}
                    </p>
                  ))}
                </section>
              )}
              {pending.length === 0
                ? <p className="ap-empty">No actions waiting. Agents can only act after you enable an action for a connector.</p>
                : pending.map(card)}
            </>
          )}
          {actions && tab === 'history' && (
            history.length === 0
              ? <p className="ap-empty">No decided actions yet.</p>
              : history.map(decidedCard)
          )}
        </div>
      </div>
    </div>
  );
}
