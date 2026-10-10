import { useCallback, useEffect, useMemo, useState } from 'react';
import { createMemoryUseApi, type MemoryUseApi, type Request } from './memoryUseApi';
import {
  actionCodes, activeRules, benefit, evidenceMix, evidenceTotal, funnel, holdoutText, noteTarget, outcomeMix, pct, rateText, sectionHint, targetLabel, targetNotePath, topLists,
  type AdherenceItem, type AdherenceResponse, type CardResponse, type KindRow, type RulesResponse, type SummaryResponse,
} from './memoryUseModel';

// Memory use: did the agent use what it was shown, did it change what it did,
// and did that help. The overview rolls the trace up; a card follows one
// memory down the funnel. Every number carries its n; "associated" and
// "caused" are kept apart (docs/MEMORY_TRACE.md).

const DAYS = [7, 30] as const;

function Hint({ error }: { error: unknown }) {
  const h = sectionHint(error);
  return <p className={'vault-note memuse-hint ' + h.kind} data-testid={`memuse-hint-${h.kind}`} role={h.kind === 'error' ? 'alert' : undefined}>{h.text}</p>;
}

type Load<T> = { data?: T; error?: unknown; done: boolean };
function useLoad<T>(fetcher: (signal: AbortSignal) => Promise<T>, deps: unknown[]): Load<T> {
  const [state, setState] = useState<Load<T>>({ done: false });
  useEffect(() => {
    const controller = new AbortController();
    setState({ done: false });
    fetcher(controller.signal).then(data => setState({ data, done: true }), error => { if (!controller.signal.aborted) setState({ error, done: true }); });
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return state;
}

export function MemoryUsePanel({ request, close, initialTarget, openNote }: { request: Request; close: () => void; initialTarget?: string; openNote?: (path: string) => void }) {
  const api = useMemo(() => createMemoryUseApi(request), [request]);
  const [target, setTarget] = useState<string | undefined>(initialTarget);
  const [days, setDays] = useState<number>(30);
  const goNote = useCallback((path: string) => { close(); openNote?.(path); }, [close, openNote]);
  return <div id="memuse-modal" className="modal" role="dialog" aria-label="Memory use" onMouseDown={e => e.currentTarget === e.target && close()}>
    <div className="modal-box banks-box memuse-box">
      <button id="memuse-close" className="icon modal-close" aria-label="Close" onClick={close}>✕</button>
      <div className="banks-head">
        {target && <button id="memuse-back" className="btn" onClick={() => setTarget(undefined)}>← Overview</button>}
        <h2 id="memuse-title" className="banks-name">{target ? targetLabel(target) : 'Memory use'}</h2>
      </div>
      <div className="memuse-days" role="group" aria-label="Window">
        {DAYS.map(d => <button key={d} className={'banks-tab' + (days === d ? ' on' : '')} aria-pressed={days === d} onClick={() => setDays(d)}>{d} days</button>)}
      </div>
      <div className="banks-body" data-testid={target ? 'memuse-card-view' : 'memuse-overview-view'}>
        {target ? <CardView api={api} target={target} days={days} openNote={openNote ? goNote : undefined} /> : <Overview api={api} days={days} select={setTarget} />}
      </div>
    </div>
  </div>;
}

// ---- overview ---------------------------------------------------------------

function Stat({ label, value, sub, id }: { label: string; value: string; sub?: string; id?: string }) {
  return <div className="memuse-stat" id={id}><div className="memuse-stat-value">{value}</div><div className="memuse-stat-label">{label}</div>{sub && <div className="memuse-stat-sub">{sub}</div>}</div>;
}

function Overview({ api, days, select }: { api: MemoryUseApi; days: number; select: (target: string) => void }) {
  const summary = useLoad(signal => api.summary(days, signal), [api, days]);
  const adherence = useLoad(signal => api.adherence(days, signal), [api, days]);
  const rules = useLoad(signal => api.rules(signal), [api]);
  const noData = adherence.done && !adherence.error && !adherence.data?.overall?.injected && !summary.data?.kinds?.some(k => k.exposures);
  return <>
    <p className="vault-note memuse-lead">How much of what agents were shown was picked up, and whether it changed what they did. Numbers are over the last {days} days.</p>
    <Headline summary={summary} adherence={adherence} />
    {noData && <p className="vault-note memuse-empty" data-testid="memuse-empty">Nothing has been shown to an agent in this window. Once an agent with the context hook installed starts a session, exposures and outcomes appear here.</p>}
    <Kinds summary={summary} />
    <Tops adherence={adherence} select={select} />
    <Rules rules={rules} select={select} />
  </>;
}

function Headline({ summary, adherence }: { summary: Load<SummaryResponse>; adherence: Load<AdherenceResponse> }) {
  const all: KindRow | undefined = summary.data?.kinds?.find(k => k.kind === '') ?? summary.data?.kinds?.[0];
  const o = adherence.data?.overall;
  const mix = outcomeMix(o ? { followed: o.followed, used: o.used, cited: o.cited, ignored: o.ignored, violated: o.violated, contradicted: o.contradicted, unknown: o.unknown, pending: o.pending } : undefined);
  const total = mix.reduce((s, m) => s + m.count, 0);
  const hold = holdoutText(summary.data?.holdout_rate, all?.causal);
  return <section className="memuse-section" data-testid="memuse-headline">
    <div className="memuse-stats">
      <Stat id="memuse-exposures" label="exposures" value={String(all?.exposures ?? o?.injected ?? 0)} sub={all ? `${all.memories} memories` : undefined} />
      <Stat id="memuse-uptake" label="uptake" value={all?.uptake.n ? pct(all.uptake.rate) : '-'} sub={all ? rateText(all.uptake) : undefined} />
      <Stat id="memuse-influence" label="influence" value={all?.influence.n ? pct(all.influence.rate) : '-'} sub={all ? rateText(all.influence) : undefined} />
      <Stat id="memuse-holdout" label="holdout" value={hold} />
    </div>
    {summary.error ? <Hint error={summary.error} /> : null}
    {adherence.error ? <Hint error={adherence.error} /> : null}
    <div className="pr-clabel">Outcome mix</div>
    {total ? <>
      <div className="memuse-bar" role="img" aria-label={mix.filter(m => m.count).map(m => `${m.key} ${m.count}`).join(', ')}>
        {mix.filter(m => m.count).map(m => <span key={m.key} className={'memuse-seg ' + m.key} style={{ flexGrow: m.count }} title={`${m.key}: ${m.count}`} />)}
      </div>
      <p className="vault-note memuse-legend">{mix.filter(m => m.count).map(m => <span key={m.key} className="memuse-key"><i className={'memuse-dot ' + m.key} />{m.key} {m.count} ({pct(m.count / total)})</span>)}</p>
    </> : adherence.done && !adherence.error ? <p className="vault-note">No outcomes recorded yet.</p> : null}
  </section>;
}

function Kinds({ summary }: { summary: Load<SummaryResponse> }) {
  const rows = (summary.data?.kinds ?? []).filter(k => k.kind !== '');
  if (!rows.length) return null;
  return <section className="memuse-section"><div className="pr-clabel">By kind</div>
    <div className="banks-scroll"><table className="usage-table memuse-table"><thead><tr><th>kind</th><th>exposures</th><th>uptake</th><th>influence</th><th>benefit</th></tr></thead>
      <tbody>{rows.map(k => <tr key={k.kind} data-kind={k.kind}><td>{k.kind}</td><td>{k.exposures}</td><td>{rateText(k.uptake)}</td><td>{rateText(k.influence)}</td><td>{benefit(k.benefit, k.causal).label}</td></tr>)}</tbody></table></div>
  </section>;
}

function Tops({ adherence, select }: { adherence: Load<AdherenceResponse>; select: (target: string) => void }) {
  if (adherence.error || !adherence.data) return null;
  const t = topLists(adherence.data.memories);
  const list = (id: string, title: string, rows: AdherenceItem[], figure: (i: AdherenceItem) => string, empty: string) =>
    <div className="memuse-top" id={id}><div className="pr-clabel">{title}</div>
      {rows.length ? <ol className="memuse-list">{rows.map(i => <li key={i.target}><button className="memuse-link" data-target={i.target} onClick={() => select(i.target)}>{targetLabel(i.target)}</button><span className="memuse-fig">{figure(i)}</span></li>)}</ol> : <p className="vault-note">{empty}</p>}
    </div>;
  return <section className="memuse-section memuse-tops" data-testid="memuse-tops">
    {list('memuse-helpful', 'Most helpful', t.helpful, i => `used ${pct(i.used_rate)} of ${i.injected}`, 'None with enough exposures yet.')}
    {list('memuse-ignored', 'Most ignored', t.ignored, i => `ignored ${i.ignored} of ${i.injected}`, 'Nothing is being ignored.')}
    {list('memuse-violated', 'Most violated', t.violated, i => `violated ${i.violated} of ${i.injected}`, 'No violations recorded.')}
  </section>;
}

function Rules({ rules, select }: { rules: Load<RulesResponse>; select: (target: string) => void }) {
  const rows = activeRules(rules.data?.checks);
  return <section className="memuse-section" data-testid="memuse-rules"><div className="pr-clabel">Active rule checks</div>
    {rules.error ? <Hint error={rules.error} /> : !rules.done ? <p className="vault-note">Loading…</p> : !rows.length ? <p className="vault-note">No compiled rule checks are active. They turn on only after a backtest finds them precise (docs/MEMORY_RULES.md).</p> :
      <div className="banks-scroll"><table className="usage-table memuse-table"><thead><tr><th>rule</th><th>acts as</th><th>precision</th><th>labelled</th></tr></thead>
        <tbody>{rows.map(r => <tr key={r.id}><td><button className="memuse-link" onClick={() => select(r.target)}>{r.rule_text.length > 90 ? r.rule_text.slice(0, 90) + '…' : r.rule_text}</button></td>
          <td>{r.status === 'enforce' ? 'asks first' : 'reminds'}</td><td>{pct(r.precision)} <small>({pct(r.ci95_low)}-{pct(r.ci95_high)})</small></td><td>{r.labelled + r.live_true + r.live_false}</td></tr>)}</tbody></table></div>}
  </section>;
}

// ---- per-memory card --------------------------------------------------------

export function CardView({ api, target, days, openNote }: { api: MemoryUseApi; target: string; days: number; openNote?: (path: string) => void }) {
  const state = useLoad(signal => api.card(target, days, signal), [api, target, days]);
  if (!state.done) return <p className="vault-note">Loading…</p>;
  if (state.error) {
    const h = sectionHint(state.error);
    return h.kind === 'none'
      ? <p className="vault-note memuse-empty" data-testid="memuse-card-empty">This memory has not been shown to an agent in the last {days} days, or you cannot read it. Nothing to trace yet.</p>
      : <Hint error={state.error} />;
  }
  return <CardBody data={state.data!} openNote={openNote} />;
}

function CardBody({ data, openNote }: { data: CardResponse; openNote?: (path: string) => void }) {
  const { card, causal } = data;
  const stages = funnel(card, causal);
  const ev = evidenceMix(card.link_evidence), evTotal = evidenceTotal(card.link_evidence);
  const b = benefit(card.benefit, causal);
  const rem = card.reminders;
  const path = targetNotePath(card.target);
  const recent = card.recent_actions ?? [];
  return <div className="memuse-card" data-testid="memuse-card">
    <p className="vault-note">{card.kind ? `${card.kind} · ` : ''}{card.target}{path && openNote ? <> · <button className="memuse-link" onClick={() => openNote(path)}>Open note</button></> : null}</p>
    <div className="pr-clabel">Funnel</div>
    <ol className="memuse-funnel" data-testid="memuse-funnel">
      {stages.map(s => <li key={s.key} data-stage={s.key}>
        <span className="memuse-stage">{s.label}</span>
        <span className="memuse-stage-n">{s.key === 'exposure' ? `n=${s.n}` : s.key === 'benefit' ? (b.label === 'insufficient data' ? 'insufficient data' : b.label) : s.n ? `${s.k}/${s.n} · ${pct(s.rate)}` : 'no data'}</span>
        {s.rate !== undefined && s.n > 0 && <span className="memuse-meter" aria-hidden="true"><i style={{ width: `${Math.round(s.rate * 100)}%` }} /></span>}
        <span className="memuse-stage-hint">{s.key === 'benefit' ? b.text : s.hint}{s.key !== 'exposure' && s.key !== 'benefit' && s.n ? ` (n=${s.n})` : ''}</span>
      </li>)}
    </ol>
    <div className="pr-clabel">How the agent took it up</div>
    {evTotal ? <ul className="memuse-evidence" data-testid="memuse-evidence">{ev.map(e => <li key={e.key} data-evidence={e.key}><b>{e.count}</b> {e.label}</li>)}</ul> : <p className="vault-note">No action was linked to this memory by tag, fingerprint, check or changed reminder.</p>}
    <p className="vault-note">Uptake {rateText(card.uptake)}. Influence {rateText(card.influence)}.</p>
    <div className="pr-clabel">Outcomes when shown</div>
    <p className="vault-note memuse-legend">{outcomeMix(card.adherence_outcomes).filter(o => o.count).map(o => <span key={o.key} className="memuse-key"><i className={'memuse-dot ' + o.key} />{o.key} {o.count}</span>)}</p>
    {rem.reminders > 0 && <p className="vault-note" data-testid="memuse-reminders">Shown as a reminder before {rem.reminders} action{rem.reminders === 1 ? '' : 's'}: {rem.unchanged} ran unchanged, {rem.changed} changed, {rem.abandoned} abandoned{rem.open ? `, ${rem.open} open` : ''}. Change rate {rateText(rem.changed_rate)}.{rem.ask ? ` Asked first ${rem.ask} time${rem.ask === 1 ? '' : 's'}: ${rem.ask_ran} ran, ${rem.ask_not_run} did not.` : ''}</p>}
    <div className="pr-clabel">Benefit <span className={'memuse-badge ' + b.label.replace(' ', '-')} data-testid="memuse-benefit-label">{b.label}</span></div>
    <p className="vault-note" data-testid="memuse-benefit">{b.text}.</p>
    {b.label === 'associated' && card.benefit.note && <p className="vault-note">{card.benefit.note}</p>}
    {causal.status !== 'estimated' && <p className="vault-note" data-testid="memuse-causal">Holdout: {causal.status}{causal.note ? `. ${causal.note}` : ''} ({causal.treated} shown, {causal.withheld} withheld).</p>}
    <div className="pr-clabel">Recent linked actions</div>
    {recent.length ? <ul className="memuse-actions" data-testid="memuse-actions">{recent.map(a => <li key={a.id}><b>{a.tool}</b> <small>{a.stage} · {new Date(a.ts * 1000).toISOString().slice(0, 16).replace('T', ' ')}</small><span>{actionCodes(a).join(', ')}</span></li>)}</ul>
      : <p className="vault-note">No linked actions yet.</p>}
  </div>;
}

/** Reachable from a note: the card for that note's memory. */
export const cardTargetForNote = noteTarget;
