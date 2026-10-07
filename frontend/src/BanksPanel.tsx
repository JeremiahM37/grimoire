import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { ApiError } from './api';
import {
  createBanksApi, createFromTemplate, UNAVAILABLE, type BankProfile, type BanksApi, type BankSummary, type DocumentDetail,
  type DocumentSummary, type EntityDetail, type EntitySummary, type Fact, type Maybe, type MentalModel, type Observation,
  type Operation, type RecallResponse, type ReflectResponse, type Request,
} from './banksApi';
import {
  armNames, buildTree, chunkId, CONFIG_KEYS, factDate, filterFacts, fmtScore, isTerminal, parseTags, progressText,
  radialLayout, rankRows, validBankId, type BankTemplate, type TreeNode,
} from './banksModel';

// Memory banks: list, profile, memories, sources, entities, observations,
// mental models, operations, and a recall/reflect playground. Every server
// call is in banksApi.ts; features the server does not have yet show as such.

type Tab = 'profile' | 'memories' | 'documents' | 'entities' | 'observations' | 'models' | 'operations' | 'playground';
const TABS: [Tab, string][] = [
  ['playground', 'Playground'], ['memories', 'Memories'], ['documents', 'Documents'], ['entities', 'Entities'],
  ['observations', 'Observations'], ['models', 'Models'], ['operations', 'Operations'], ['profile', 'Profile'],
];

const message = (e: unknown) => (e instanceof Error ? e.message : String(e));
function ErrorText({ error }: { error: unknown }) {
  return error ? <p className="vault-note banks-error" role="alert">{message(error)}</p> : null;
}
function Unavailable({ what }: { what: string }) {
  return <p className="vault-note banks-unavailable" data-testid="unavailable">{what} — not available on this server yet.</p>;
}
function Badges({ f }: { f: { authority?: string; disputed_by?: string; doc_removed?: boolean } }) {
  return <>
    {f.authority === 'human' && <span className="banks-badge human" title="A person wrote or corrected this">human</span>}
    {f.disputed_by && <span className="banks-badge disputed" title={`Contradicts ${f.disputed_by}`}>disputed</span>}
    {f.doc_removed && <span className="banks-badge removed" title="Its source text no longer exists">source removed</span>}
  </>;
}

export function BanksPanel({ request, close }: { request: Request; close: () => void }) {
  const api = useMemo(() => createBanksApi(request), [request]);
  const [banks, setBanks] = useState<BankSummary[]>();
  const [bank, setBank] = useState<string>();
  const [tab, setTab] = useState<Tab>('playground');
  const [error, setError] = useState<unknown>();
  const load = useCallback(() => api.list().then(setBanks).catch(setError), [api]);
  useEffect(() => { void load(); }, [load]);
  return <div id="banks-modal" className="modal" role="dialog" aria-label="Memory banks" onMouseDown={e => e.currentTarget === e.target && close()}>
    <div className="modal-box banks-box">
      <button id="banks-close" className="icon modal-close" aria-label="Close" onClick={close}>✕</button>
      {!bank ? <>
        <h2 id="banks-title">Memory banks</h2>
        <div className="banks-body"><ErrorText error={error} />
          <BankList banks={banks} select={id => { setBank(id); setTab('playground'); }} />
          <CreateBank api={api} created={id => { void load(); setBank(id); setTab('profile'); }} />
        </div>
      </> : <>
        <div className="banks-head">
          <button id="banks-back" className="btn" onClick={() => { setBank(undefined); void load(); }}>← Banks</button>
          <h2 id="banks-title" className="banks-name">{bank}</h2>
        </div>
        <nav className="banks-tabs" role="tablist">
          {TABS.map(([key, label]) => <button key={key} role="tab" aria-selected={tab === key} data-tab={key}
            className={'banks-tab' + (tab === key ? ' on' : '')} onClick={() => setTab(key)}>{label}</button>)}
        </nav>
        <div className="banks-body" data-testid={`banks-tab-${tab}`}>
          {tab === 'profile' && <ProfileTab api={api} bank={bank} deleted={() => { setBank(undefined); void load(); }} />}
          {tab === 'memories' && <MemoriesTab api={api} bank={bank} />}
          {tab === 'documents' && <DocumentsTab api={api} bank={bank} />}
          {tab === 'entities' && <EntitiesTab api={api} bank={bank} />}
          {tab === 'observations' && <ObservationsTab api={api} bank={bank} />}
          {tab === 'models' && <ModelsTab api={api} bank={bank} />}
          {tab === 'operations' && <OperationsTab api={api} bank={bank} />}
          {tab === 'playground' && <PlaygroundTab api={api} bank={bank} />}
        </div>
      </>}
    </div>
  </div>;
}

// ---- bank list and creation ------------------------------------------------

function BankList({ banks, select }: { banks?: BankSummary[]; select: (id: string) => void }) {
  if (!banks) return <p className="vault-note">Loading…</p>;
  if (!banks.length) return <p className="vault-note">No banks yet. A bank is created the first time something is retained into it, or below.</p>;
  return <div className="banks-list" id="banks-list">
    {banks.map(b => <button key={b.bank_id} className="banks-row" data-bank={b.bank_id} onClick={() => select(b.bank_id)}>
      <span className="banks-row-name">{b.name && b.name !== b.bank_id ? <>{b.name} <small>{b.bank_id}</small></> : b.bank_id}</span>
      <span className="banks-row-meta">{b.facts} facts · {b.documents} documents{b.updated ? ` · ${b.updated.slice(0, 10)}` : ''}</span>
    </button>)}
  </div>;
}

function CreateBank({ api, created }: { api: BanksApi; created: (id: string) => void }) {
  const [templates, setTemplates] = useState<BankTemplate[]>([]);
  const [id, setId] = useState(''), [name, setName] = useState(''), [mission, setMission] = useState(''), [template, setTemplate] = useState('');
  const [error, setError] = useState<unknown>(), [busy, setBusy] = useState(false);
  useEffect(() => { void api.templates().then(setTemplates).catch(() => setTemplates([])); }, [api]);
  const chosen = templates.find(t => t.id === template);
  const submit = async () => {
    if (!validBankId(id)) return setError('Bank ids are lowercase letters, digits and . _ : - (at most 64).');
    setBusy(true); setError(undefined);
    try { await createFromTemplate(api, id, chosen, { name, mission }); created(id); } catch (e) { setError(e); } finally { setBusy(false); }
  };
  return <form className="banks-form" id="banks-create" onSubmit={e => { e.preventDefault(); void submit(); }}>
    <div className="pr-clabel">New bank</div>
    <label>Id <input name="bank_id" value={id} onChange={e => setId(e.target.value.trim())} placeholder="coding-agent:my-repo" required /></label>
    <label>Name <input name="name" value={name} onChange={e => setName(e.target.value)} placeholder="optional" /></label>
    <label>Template <select name="template" value={template} onChange={e => setTemplate(e.target.value)}>
      <option value="">None</option>
      {templates.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
    </select></label>
    {chosen && <p className="vault-note">{chosen.description}</p>}
    <label>Mission <textarea name="mission" rows={2} value={mission} onChange={e => setMission(e.target.value)} placeholder={chosen?.manifest.bank?.mission || 'What this bank is for'} /></label>
    <ErrorText error={error} />
    <button type="submit" className="btn" disabled={busy}>Create bank</button>
  </form>;
}

// ---- profile -----------------------------------------------------------------

function ProfileTab({ api, bank, deleted }: { api: BanksApi; bank: string; deleted: () => void }) {
  const [p, setP] = useState<BankProfile>(), [error, setError] = useState<unknown>(), [saved, setSaved] = useState('');
  const [draft, setDraft] = useState('');
  useEffect(() => { void api.profile(bank).then(setP).catch(setError); }, [api, bank]);
  if (!p) return <><ErrorText error={error} /><p className="vault-note">Loading…</p></>;
  const edit = (patch: Partial<BankProfile>) => { setP({ ...p, ...patch }); setSaved(''); };
  const save = async () => {
    setError(undefined);
    try {
      setP(await api.update(bank, { name: p.name, mission: p.mission, retain_mission: p.retain_mission, disposition: p.disposition, directives: p.directives, config: p.config }));
      setSaved('Saved');
    } catch (e) { setError(e); }
  };
  const setConfig = (key: string, value: string) => {
    const config = { ...p.config };
    // An empty value tells the server to remove the setting.
    config[key] = value;
    edit({ config });
  };
  const trait = (key: keyof BankProfile['disposition'], label: string, low: string, high: string) =>
    <label className="banks-slider">{label} <span className="banks-slider-ends">{low}</span>
      <input type="range" min={1} max={5} step={1} name={key} value={p.disposition[key]} onChange={e => edit({ disposition: { ...p.disposition, [key]: Number(e.target.value) } })} />
      <span className="banks-slider-ends">{high}</span> <b>{p.disposition[key]}</b></label>;
  return <form className="banks-form" id="banks-profile" onSubmit={e => { e.preventDefault(); void save(); }}>
    <label>Name <input name="name" value={p.name} onChange={e => edit({ name: e.target.value })} /></label>
    <label>Mission <textarea name="mission" rows={3} value={p.mission} onChange={e => edit({ mission: e.target.value })} /></label>
    <label>Retain mission <textarea name="retain_mission" rows={3} value={p.retain_mission} onChange={e => edit({ retain_mission: e.target.value })} placeholder="What extraction should keep" /></label>
    <div className="pr-clabel">Disposition</div>
    {trait('skepticism', 'Skepticism', 'trusting', 'doubting')}
    {trait('literalism', 'Literalism', 'reads between lines', 'literal')}
    {trait('empathy', 'Empathy', 'facts only', 'feelings matter')}
    <div className="pr-clabel">Directives</div>
    <div className="banks-directives">
      {p.directives.map((d, i) => <div className="banks-directive" key={d.id || i}>
        <input aria-label={`Directive ${i + 1}`} value={d.text} onChange={e => edit({ directives: p.directives.map((x, j) => j === i ? { ...x, text: e.target.value } : x) })} />
        <button type="button" className="btn" onClick={() => edit({ directives: p.directives.filter((_, j) => j !== i) })}>Remove</button>
      </div>)}
      <div className="banks-directive">
        <input id="banks-new-directive" placeholder="Add a standing rule, e.g. Never reveal account numbers" value={draft} onChange={e => setDraft(e.target.value)} />
        <button type="button" className="btn" disabled={!draft.trim()} onClick={() => { edit({ directives: [...p.directives, { text: draft.trim() }] }); setDraft(''); }}>Add</button>
      </div>
    </div>
    <div className="pr-clabel">Settings</div>
    <div className="banks-config">
      {CONFIG_KEYS.map(k => <label key={k.key}>{k.label}
        <select name={k.key} value={p.config[k.key] || ''} onChange={e => setConfig(k.key, e.target.value)}>
          <option value="">default</option>{k.values.map(v => <option key={v} value={v}>{v}</option>)}
        </select></label>)}
    </div>
    <ErrorText error={error} />
    <div className="banks-actions">
      <button type="submit" className="btn" id="banks-profile-save">Save profile</button><span className="vault-note">{saved}</span>
      <button type="button" className="btn banks-danger" onClick={() => {
        if (confirm(`Delete bank ${bank} and everything in it? Its files go to history.`)) void api.remove(bank).then(deleted).catch(setError);
      }}>Delete bank</button>
    </div>
  </form>;
}

// ---- memories ------------------------------------------------------------------

const PAGE = 50;

function FactRow({ f, extra, children }: { f: Fact; extra?: ReactNode; children?: ReactNode }) {
  return <tr data-fact={f.id} className={f.disputed_by ? 'banks-disputed' : ''}>
    <td className="banks-fact-text">{f.text} <Badges f={f} />{children}</td>
    <td>{f.type}</td>
    <td>{factDate(f) || '—'}</td>
    <td>{(f.tags || []).join(', ')}</td>
    {extra}
  </tr>;
}

function MemoriesTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [q, setQ] = useState(''), [type, setType] = useState(''), [authority, setAuthority] = useState('');
  const [tag, setTag] = useState(''), [from, setFrom] = useState(''), [to, setTo] = useState('');
  const [offset, setOffset] = useState(0);
  const [data, setData] = useState<{ items: Fact[]; total: number }>(), [error, setError] = useState<unknown>();
  const load = useCallback(() => api.memories(bank, { q, type, authority, limit: PAGE, offset }).then(setData).catch(setError), [api, bank, q, type, authority, offset]);
  useEffect(() => { void load(); }, [load]);
  const remove = async (f: Fact) => {
    try { await api.deleteMemory(bank, f.id); }
    catch (e) {
      if (!(e instanceof ApiError && e.status === 409)) return setError(e);
      if (!confirm('A person wrote or edited this fact. Delete it anyway?')) return;
      try { await api.deleteMemory(bank, f.id, true); } catch (e2) { return setError(e2); }
    }
    void load();
  };
  const rows = filterFacts(data?.items || [], { tag, from, to });
  return <>
    <div className="banks-filters">
      <input aria-label="Search facts" placeholder="Search text" value={q} onChange={e => { setQ(e.target.value); setOffset(0); }} />
      <select aria-label="Type" value={type} onChange={e => { setType(e.target.value); setOffset(0); }}>
        <option value="">All types</option><option value="world">world</option><option value="experience">experience</option><option value="observation">observation</option>
      </select>
      <select aria-label="Author" value={authority} onChange={e => { setAuthority(e.target.value); setOffset(0); }}>
        <option value="">Anyone</option><option value="human">Written by a person</option><option value="agent">Extracted by a model</option>
      </select>
      <input aria-label="Tag" placeholder="Tag" value={tag} onChange={e => setTag(e.target.value)} />
      <label className="banks-date">From <input type="date" value={from} onChange={e => setFrom(e.target.value)} /></label>
      <label className="banks-date">To <input type="date" value={to} onChange={e => setTo(e.target.value)} /></label>
    </div>
    <ErrorText error={error} />
    {!data ? <p className="vault-note">Loading…</p> : <>
      <p className="vault-note">{data.total} fact(s){(tag || from || to) ? `, ${rows.length} on this page match the tag/date filter` : ''}.</p>
      <div className="banks-scroll"><table className="usage-table banks-table" id="banks-memories">
        <thead><tr><th>fact</th><th>type</th><th>date</th><th>tags</th><th /></tr></thead>
        <tbody>{rows.map(f => <FactRow key={f.id} f={f} extra={<td><button className="btn banks-small" onClick={() => void remove(f)}>Delete</button></td>} />)}</tbody>
      </table></div>
      <div className="banks-pager">
        <button className="btn" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>Previous</button>
        <span className="vault-note">{data.total ? `${offset + 1}–${Math.min(offset + PAGE, data.total)} of ${data.total}` : ''}</span>
        <button className="btn" disabled={offset + PAGE >= data.total} onClick={() => setOffset(offset + PAGE)}>Next</button>
      </div>
    </>}
  </>;
}

// ---- documents -------------------------------------------------------------------

function DocumentsTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [docs, setDocs] = useState<{ items: DocumentSummary[]; total: number }>(), [error, setError] = useState<unknown>();
  const [open, setOpen] = useState<string>();
  const load = useCallback(() => api.documents(bank).then(setDocs).catch(setError), [api, bank]);
  useEffect(() => { void load(); }, [load]);
  if (open) return <DocumentView api={api} bank={bank} id={open} back={() => { setOpen(undefined); void load(); }} />;
  return <>
    <ErrorText error={error} />
    {!docs ? <p className="vault-note">Loading…</p> : !docs.items.length ? <p className="vault-note">No documents yet.</p> :
      <div className="banks-scroll"><table className="usage-table banks-table" id="banks-documents">
        <thead><tr><th>document</th><th>when</th><th className="num">chunks</th><th className="num">facts</th><th>tags</th></tr></thead>
        <tbody>{docs.items.map(d => <tr key={d.document_id} data-doc={d.document_id}>
          <td><a className="wikilink" onClick={() => setOpen(d.document_id)}>{d.document_id}</a>{d.context && <div className="banks-sub">{d.context}</div>}</td>
          <td>{(d.timestamp || d.updated || '').slice(0, 10) || '—'}</td>
          <td className="num">{d.chunks}</td><td className="num">{d.facts}</td><td>{d.tags.join(', ')}</td>
        </tr>)}</tbody>
      </table></div>}
    <RetainForm api={api} bank={bank} done={() => void load()} />
  </>;
}

function DocumentView({ api, bank, id, back }: { api: BanksApi; bank: string; id: string; back: () => void }) {
  const [doc, setDoc] = useState<DocumentDetail>(), [chunks, setChunks] = useState<{ index: number; text: string }[]>(), [error, setError] = useState<unknown>();
  useEffect(() => {
    void (async () => {
      const d = await api.document(bank, id);
      setDoc(d);
      const count = d.chunk_hashes?.length || 0;
      const got = await Promise.all(Array.from({ length: count }, (_, i) =>
        api.chunk(bank, chunkId(bank, id, i)).then(c => ({ index: i, text: c.text })).catch(() => ({ index: i, text: '' }))));
      setChunks(got);
    })().catch(setError);
  }, [api, bank, id]);
  const remove = async () => {
    if (!confirm(`Delete document ${id}? Facts a person wrote are kept and marked as having lost their source.`)) return;
    try { await api.deleteDocument(bank, id); back(); } catch (e) { setError(e); }
  };
  return <div id="banks-document">
    <div className="banks-actions"><button className="btn" onClick={back}>← Documents</button><button className="btn banks-danger" onClick={() => void remove()}>Delete document</button></div>
    <ErrorText error={error} />
    {!doc ? <p className="vault-note">Loading…</p> : <>
      <h3 className="banks-h3">{doc.document_id}</h3>
      <p className="vault-note">{[doc.timestamp?.slice(0, 19).replace('T', ' '), doc.context, doc.tags?.length ? `tags: ${doc.tags.join(', ')}` : '', doc.path].filter(Boolean).join(' · ')}</p>
      {(chunks?.length ? chunks : [{ index: 0, text: doc.content || '' }]).map(c => {
        const facts = doc.facts.filter(f => !f.chunk_id || f.chunk_id === chunkId(bank, id, c.index));
        return <div className="inspect-chunk banks-chunk" key={c.index} data-chunk={c.index}>
          <div className="ic-head"><span>chunk {c.index}</span><span className="ic-score">{facts.length} fact(s)</span></div>
          <pre className="banks-pre">{c.text}</pre>
          {facts.length > 0 && <ul className="banks-facts">{facts.map(f => <li key={f.id}>{f.text} <Badges f={f} /></li>)}</ul>}
        </div>;
      })}
    </>}
  </div>;
}

function RetainForm({ api, bank, done }: { api: BanksApi; bank: string; done: () => void }) {
  const [content, setContent] = useState(''), [doc, setDoc] = useState(''), [context, setContext] = useState(''), [tags, setTags] = useState(''), [mode, setMode] = useState('');
  const [status, setStatus] = useState(''), [error, setError] = useState<unknown>(), [busy, setBusy] = useState(false);
  const submit = async () => {
    setBusy(true); setError(undefined); setStatus('');
    try {
      const r = await api.retain(bank, [{ content, document_id: doc || undefined, context: context || undefined, tags: tags ? parseTags(tags) : undefined }], mode);
      setStatus(`Retained: ${(r.documents || []).map(d => `${d.document_id} (${d.facts} facts)`).join(', ')}`);
      setContent(''); done();
    } catch (e) { setError(e); } finally { setBusy(false); }
  };
  return <form className="banks-form" id="banks-retain" onSubmit={e => { e.preventDefault(); void submit(); }}>
    <div className="pr-clabel">Retain content</div>
    <textarea name="content" rows={4} value={content} onChange={e => setContent(e.target.value)} placeholder="A transcript, a document, notes…" required />
    <div className="banks-row-fields">
      <input name="document_id" placeholder="document id (optional)" value={doc} onChange={e => setDoc(e.target.value)} />
      <input name="context" placeholder="context" value={context} onChange={e => setContext(e.target.value)} />
      <input name="tags" placeholder="tags, comma separated" value={tags} onChange={e => setTags(e.target.value)} />
      <select name="mode" aria-label="Extraction mode" value={mode} onChange={e => setMode(e.target.value)}>
        <option value="">bank default</option><option value="concise">concise</option><option value="verbatim">verbatim</option><option value="chunks">chunks (no model)</option>
      </select>
    </div>
    <ErrorText error={error} /><p className="vault-note">{status}</p>
    <button type="submit" className="btn" disabled={busy || !content.trim()}>{busy ? 'Retaining…' : 'Retain'}</button>
  </form>;
}

// ---- entities ------------------------------------------------------------------------

function EntityGraph({ entities, detail, select }: { entities: EntitySummary[]; detail?: EntityDetail; select: (id: string) => void }) {
  const size = 260;
  const nodes = detail
    ? [{ id: detail.entity_id, label: detail.canonical_name, weight: detail.mention_count }, ...detail.related.slice(0, 14).map(r => ({ id: r.entity_id, label: r.canonical_name, weight: r.mention_count }))]
    : entities.slice(0, 16).map(e => ({ id: e.entity_id, label: e.canonical_name, weight: e.mention_count }));
  const points = radialLayout(nodes, detail?.entity_id, size);
  const centre = points.find(p => p.center);
  return <svg className="banks-graph" viewBox={`0 0 ${size} ${size}`} role="img" aria-label="Entity graph" data-testid="entity-graph">
    {centre && points.filter(p => !p.center).map(p => <line key={'l' + p.id} x1={centre.x} y1={centre.y} x2={p.x} y2={p.y} className="banks-edge" />)}
    {points.map(p => <g key={p.id} className={'banks-node' + (p.center ? ' center' : '')} onClick={() => select(p.id)} tabIndex={0}
      onKeyDown={e => e.key === 'Enter' && select(p.id)}>
      <circle cx={p.x} cy={p.y} r={p.r} />
      <text x={p.x} y={p.y + p.r + 10} textAnchor="middle">{p.label.length > 16 ? p.label.slice(0, 15) + '…' : p.label}</text>
    </g>)}
  </svg>;
}

function EntitiesTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [q, setQ] = useState(''), [entities, setEntities] = useState<EntitySummary[]>(), [detail, setDetail] = useState<EntityDetail>(), [error, setError] = useState<unknown>();
  useEffect(() => { void api.entities(bank, q).then(setEntities).catch(setError); }, [api, bank, q]);
  const select = (id: string) => void api.entity(bank, id).then(setDetail).catch(setError);
  return <>
    <div className="banks-filters"><input aria-label="Search entities" placeholder="Search entities" value={q} onChange={e => setQ(e.target.value)} />
      {detail && <button className="btn" onClick={() => setDetail(undefined)}>Show top entities</button>}</div>
    <ErrorText error={error} />
    {!entities ? <p className="vault-note">Loading…</p> : !entities.length ? <p className="vault-note">No entities yet.</p> :
      <div className="banks-split">
        <div className="banks-scroll banks-entity-list"><table className="usage-table banks-table" id="banks-entities">
          <thead><tr><th>entity</th><th className="num">mentions</th><th>last seen</th></tr></thead>
          <tbody>{entities.map(e => <tr key={e.entity_id} className={detail?.entity_id === e.entity_id ? 'on' : ''}>
            <td><a className="wikilink" onClick={() => select(e.entity_id)}>{e.canonical_name}</a></td>
            <td className="num">{e.mention_count}</td><td>{(e.last_seen || '').slice(0, 10) || '—'}</td></tr>)}</tbody>
        </table></div>
        <div className="banks-graph-wrap">
          <EntityGraph entities={entities} detail={detail} select={select} />
          {detail && <div id="banks-entity">
            <div className="pr-clabel">{detail.canonical_name} · {detail.mention_count} mention(s)</div>
            <ul className="banks-facts">{detail.facts.map(f => <li key={f.id}>{f.text} <Badges f={f} /></li>)}</ul>
          </div>}
        </div>
      </div>}
  </>;
}

// ---- observations ------------------------------------------------------------------------

function ObservationsTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [data, setData] = useState<Maybe<{ items: Observation[]; total: number }>>(), [q, setQ] = useState(''), [error, setError] = useState<unknown>(), [note, setNote] = useState('');
  const load = useCallback(() => api.observations(bank, { q, limit: 100 }).then(setData).catch(setError), [api, bank, q]);
  useEffect(() => { void load(); }, [load]);
  if (data === UNAVAILABLE) return <Unavailable what="Observations (consolidated knowledge)" />;
  const consolidate = async () => {
    const r = await api.consolidate(bank).catch(e => { setError(e); return undefined; });
    if (r === UNAVAILABLE) setNote('Consolidation is not available on this server yet.');
    else if (r) setNote(`Consolidation queued (operation ${r.operation_id}).`);
  };
  return <>
    <div className="banks-filters"><input aria-label="Search observations" placeholder="Search" value={q} onChange={e => setQ(e.target.value)} />
      <button className="btn" onClick={() => void consolidate()}>Consolidate now</button></div>
    <ErrorText error={error} /><p className="vault-note">{note}</p>
    {!data ? <p className="vault-note">Loading…</p> : !data.items.length ? <p className="vault-note">No observations yet.</p> :
      data.items.map(o => <div className="inspect-chunk" key={o.id} data-observation={o.id}>
        <div className="ic-head"><span>{o.proof_count ?? o.evidence?.length ?? 0} supporting fact(s) <Badges f={o} /></span><span className="ic-score">{(o.updated || '').slice(0, 10)}</span></div>
        <div className="ic-text">{o.text}</div>
        {o.tags?.length ? <div className="banks-sub">tags: {o.tags.join(', ')}</div> : null}
      </div>)}
  </>;
}

// ---- mental models ------------------------------------------------------------------------

function ModelTree({ nodes, selected, select }: { nodes: TreeNode<MentalModel>[]; selected?: string; select: (id: string) => void }) {
  return <ul className="banks-tree">{nodes.map(n => <li key={n.path}>
    {n.item ? <a className={'wikilink' + (selected === n.item.id ? ' on' : '')} onClick={() => select(n.item!.id)}>
      {n.item.name || n.name}{n.item.pending_proposal && <span className="banks-badge proposal">proposal</span>}{n.item.is_stale && <span className="banks-badge stale">stale</span>}</a>
      : <span className="banks-folder">{n.name}/</span>}
    {n.children.length > 0 && <ModelTree nodes={n.children} selected={selected} select={select} />}
  </li>)}</ul>;
}

function ModelsTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [data, setData] = useState<Maybe<{ items: MentalModel[]; total: number }>>(), [error, setError] = useState<unknown>();
  const [selected, setSelected] = useState<string>(), [model, setModel] = useState<MentalModel>(), [note, setNote] = useState('');
  const [name, setName] = useState(''), [mid, setMid] = useState(''), [query, setQuery] = useState('');
  const load = useCallback(() => api.mentalModels(bank).then(setData).catch(setError), [api, bank]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => {
    if (!selected) return setModel(undefined);
    void api.mentalModel(bank, selected).then(m => setModel(m === UNAVAILABLE ? undefined : m)).catch(setError);
  }, [api, bank, selected]);
  if (data === UNAVAILABLE) return <Unavailable what="Mental models" />;
  const act = async (fn: () => Promise<unknown>, done: string) => {
    setError(undefined);
    try { const r = await fn(); setNote(r === UNAVAILABLE ? 'Not available on this server yet.' : done); void load(); if (selected) setModel((await api.mentalModel(bank, selected)) as MentalModel); } catch (e) { setError(e); }
  };
  return <>
    <ErrorText error={error} /><p className="vault-note">{note}</p>
    {!data ? <p className="vault-note">Loading…</p> : <div className="banks-split">
      <div className="banks-tree-wrap">{data.items.length ? <ModelTree nodes={buildTree(data.items)} selected={selected} select={setSelected} /> : <p className="vault-note">No mental models yet.</p>}
        <form className="banks-form" onSubmit={e => { e.preventDefault(); void act(() => api.createMentalModel(bank, { id: mid || undefined, name, source_query: query, trigger: { refresh_after_consolidation: true } }), 'Created; its first refresh is queued.').then(() => { setName(''); setMid(''); setQuery(''); }); }}>
          <div className="pr-clabel">New mental model</div>
          <input placeholder="Name" value={name} onChange={e => setName(e.target.value)} required />
          <input placeholder="id (optional; folders with /)" value={mid} onChange={e => setMid(e.target.value)} />
          <textarea rows={2} placeholder="The question it answers" value={query} onChange={e => setQuery(e.target.value)} required />
          <button type="submit" className="btn">Create</button>
        </form>
      </div>
      <div className="banks-model">{model ? <>
        <h3 className="banks-h3">{model.name} <Badges f={model} /></h3>
        <p className="vault-note">{model.source_query}{model.last_refreshed_at ? ` · refreshed ${model.last_refreshed_at.slice(0, 16).replace('T', ' ')}` : ''}</p>
        <div className="banks-actions">
          <button className="btn" onClick={() => void act(() => api.refreshMentalModel(bank, model.id), 'Refresh queued.')}>Refresh</button>
          <button className="btn banks-danger" onClick={() => { if (confirm(`Delete ${model.name}?`)) void act(() => api.deleteMentalModel(bank, model.id), 'Deleted.').then(() => setSelected(undefined)); }}>Delete</button>
        </div>
        {model.pending_proposal ? <div className="banks-proposal" data-testid="proposal">
          <p className="vault-note">A refresh proposed new text, but a person has edited this model since it was last refreshed. Your text stays until you accept.</p>
          <div className="banks-compare"><div><div className="pr-clabel">Current</div><pre className="banks-pre">{model.content}</pre></div>
            <div><div className="pr-clabel">Proposed</div><pre className="banks-pre">{model.pending_proposal.content}</pre></div></div>
          <div className="banks-actions">
            <button className="btn" onClick={() => void act(() => api.acceptProposal(bank, model.id), 'Proposal accepted.')}>Accept proposal</button>
            <button className="btn" onClick={() => void act(() => api.rejectProposal(bank, model.id), 'Proposal rejected.')}>Keep mine</button>
          </div>
        </div> : <pre className="banks-pre">{model.content || '(empty — refresh to generate)'}</pre>}
      </> : <p className="vault-note">Choose a model to view it.</p>}</div>
    </div>}
  </>;
}

// ---- operations ------------------------------------------------------------------------------

function OperationsTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [data, setData] = useState<Maybe<{ operations: Operation[]; total: number }>>(), [status, setStatus] = useState(''), [error, setError] = useState<unknown>();
  const load = useCallback(() => api.operations(bank, { status, limit: 50 }).then(setData).catch(setError), [api, bank, status]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => {
    if (!data || data === UNAVAILABLE || data.operations.every(o => isTerminal(o.status))) return;
    const timer = setInterval(() => void load(), 3000);
    return () => clearInterval(timer);
  }, [data, load]);
  if (data === UNAVAILABLE) return <Unavailable what="Background operations" />;
  return <>
    <div className="banks-filters"><select aria-label="Status" value={status} onChange={e => setStatus(e.target.value)}>
      <option value="">All</option>{['pending', 'processing', 'completed', 'failed', 'cancelled'].map(s => <option key={s}>{s}</option>)}
    </select><button className="btn" onClick={() => void load()}>Refresh</button></div>
    <ErrorText error={error} />
    {!data ? <p className="vault-note">Loading…</p> : !data.operations.length ? <p className="vault-note">No operations.</p> :
      <div className="banks-scroll"><table className="usage-table banks-table" id="banks-operations">
        <thead><tr><th>operation</th><th>status</th><th>progress</th><th>created</th><th /></tr></thead>
        <tbody>{data.operations.map(o => <tr key={o.id}>
          <td>{o.operation_type}<div className="banks-sub">{o.id}</div>{o.error_message && <div className="banks-sub banks-error">{o.error_message}</div>}</td>
          <td><span className={`banks-chip ${o.status}`}>{o.status}</span></td>
          <td>{progressText(o.progress)}</td>
          <td>{(o.created_at || '').slice(0, 16).replace('T', ' ')}</td>
          <td>{!isTerminal(o.status) && <button className="btn banks-small" onClick={() => void api.cancelOperation(bank, o.id).then(load).catch(setError)}>Cancel</button>}</td>
        </tr>)}</tbody>
      </table></div>}
  </>;
}

// ---- playground ---------------------------------------------------------------------------------

function PlaygroundTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [query, setQuery] = useState(''), [budget, setBudget] = useState('mid'), [maxTokens, setMaxTokens] = useState(4096);
  const [types, setTypes] = useState<string[]>(['world', 'experience', 'observation']), [tags, setTags] = useState('');
  const [recall, setRecall] = useState<RecallResponse>(), [reflect, setReflect] = useState<Maybe<ReflectResponse>>();
  const [error, setError] = useState<unknown>(), [busy, setBusy] = useState('');
  const params = () => ({ query, budget, max_tokens: maxTokens, types, tags: tags ? parseTags(tags) : undefined });
  const run = async (kind: 'recall' | 'reflect') => {
    setBusy(kind); setError(undefined);
    try {
      if (kind === 'recall') { setReflect(undefined); setRecall(await api.recall(bank, { ...params(), trace: true })); }
      else { setRecall(undefined); setReflect(await api.reflect(bank, params())); }
    } catch (e) { setError(e); } finally { setBusy(''); }
  };
  return <>
    <form className="banks-form" id="banks-playground" onSubmit={e => { e.preventDefault(); void run('recall'); }}>
      <textarea id="banks-query" name="query" rows={2} value={query} onChange={e => setQuery(e.target.value)} placeholder="Ask the bank something…" required />
      <div className="banks-row-fields">
        <label>Budget <select name="budget" value={budget} onChange={e => setBudget(e.target.value)}><option>low</option><option>mid</option><option>high</option></select></label>
        <label>Max tokens <input name="max_tokens" type="number" min={0} max={65536} value={maxTokens} onChange={e => setMaxTokens(Number(e.target.value))} /></label>
        <input name="tags" placeholder="tags" value={tags} onChange={e => setTags(e.target.value)} />
      </div>
      <div className="banks-row-fields">{['world', 'experience', 'observation'].map(t => <label key={t} className="chk">
        <input type="checkbox" checked={types.includes(t)} onChange={e => setTypes(e.target.checked ? [...types, t] : types.filter(x => x !== t))} />{t}</label>)}</div>
      <div className="banks-actions">
        <button type="submit" id="banks-recall" className="btn" disabled={!!busy || !query.trim()}>{busy === 'recall' ? 'Recalling…' : 'Recall'}</button>
        <button type="button" id="banks-reflect" className="btn" disabled={!!busy || !query.trim()} onClick={() => void run('reflect')}>{busy === 'reflect' ? 'Reflecting…' : 'Reflect'}</button>
      </div>
    </form>
    <ErrorText error={error} />
    {recall && <RecallView r={recall} />}
    {reflect === UNAVAILABLE ? <Unavailable what="Reflect (a synthesised answer with citations)" /> : reflect && <ReflectView r={reflect} />}
  </>;
}

function RecallView({ r }: { r: RecallResponse }) {
  const t = r.trace;
  const arms = armNames(t);
  const short = (id: string) => id.length > 10 ? id.slice(0, 9) + '…' : id;
  return <div id="banks-recall-results">
    <div className="pr-clabel">{r.results.length} result(s)</div>
    {!r.results.length && <p className="vault-note">Nothing in this bank matched.</p>}
    <div className="banks-scroll"><table className="usage-table banks-table">
      <thead><tr><th>#</th><th>fact</th><th className="num">final</th><th className="num">rerank</th><th className="num">semantic</th><th className="num">keyword</th></tr></thead>
      <tbody>{r.results.map((f, i) => <tr key={f.id} data-result={f.id} className={f.disputed_by ? 'banks-disputed' : ''}>
        <td>{i + 1}</td>
        <td className="banks-fact-text">{f.text} <Badges f={f} /><div className="banks-sub">{[f.type, factDate(f), f.document_id, (f.entities || []).join(', ')].filter(Boolean).join(' · ')}</div></td>
        <td className="num">{fmtScore(f.scores?.final)}</td><td className="num">{fmtScore(f.scores?.reranker)}</td>
        <td className="num">{fmtScore(f.scores?.semantic)}</td><td className="num">{fmtScore(f.scores?.keyword)}</td>
      </tr>)}</tbody>
    </table></div>
    {t && <details className="banks-trace" id="banks-trace" open>
      <summary>Trace · {t.tokens_used} tokens used · {t.fused_candidates} fused candidates{t.reranked ? ' · reranked' : ''}</summary>
      <p className="vault-note" data-testid="trace-summary">
        Tokens used: <b>{t.tokens_used}</b> · bank facts: {t.bank_facts} · candidates per arm: {t.thinking_budget}
        {t.temporal_window && <> · time window {t.temporal_window.start.slice(0, 10)} to {t.temporal_window.end.slice(0, 10)}</>}
        {t.keyword_terms?.length ? <> · keyword terms: {t.keyword_terms.join(', ')}</> : null}
      </p>
      <div className="pr-clabel">Rank in each arm</div>
      <div className="banks-scroll"><table className="usage-table banks-table" id="banks-ranks">
        <thead><tr><th>result</th>{arms.map(a => <th key={a} className="num">{a} ({t.arms?.[a]?.length ?? 0})</th>)}</tr></thead>
        <tbody>{rankRows(r.results.map(f => f.id), t.ranks, arms).map((row, i) => <tr key={row.id}>
          <td title={row.id}>{i + 1}. {short(row.id)}</td>{row.ranks.map((rank, j) => <td key={j} className="num">{rank ?? '—'}</td>)}</tr>)}</tbody>
      </table></div>
      <div className="pr-clabel">Timings</div>
      <p className="vault-note">{Object.entries(t.timings_ms || {}).map(([k, v]) => `${k} ${v.toFixed(1)} ms`).join(' · ')}</p>
      {t.disputes && Object.keys(t.disputes).length > 0 && <p className="vault-note">Disputes: {Object.entries(t.disputes).map(([a, b]) => `${short(a)} contradicts ${short(b)}`).join('; ')}</p>}
    </details>}
    {r.chunks && Object.keys(r.chunks).length > 0 && <>
      <div className="pr-clabel">Source chunks</div>
      {Object.values(r.chunks).map(c => <div className="inspect-chunk" key={c.id}><div className="ic-head"><span>{c.document_id} · chunk {c.chunk_index}</span></div><pre className="banks-pre">{c.text}</pre></div>)}
    </>}
  </div>;
}

function ReflectView({ r }: { r: ReflectResponse }) {
  const b = r.based_on || {};
  return <div id="banks-reflect-results">
    <div className="pr-clabel">Answer</div>
    <div className="banks-answer">{r.text}</div>
    {r.usage && <p className="vault-note">{r.usage.input_tokens ?? 0} tokens in · {r.usage.output_tokens ?? 0} out</p>}
    <div className="pr-clabel">Based on</div>
    {(b.memories || []).map(f => <div className="inspect-chunk" key={f.id}><div className="ic-head"><span>fact {f.id} <Badges f={f} /></span></div><div className="ic-text">{f.text}</div></div>)}
    {(b.observations || []).map(o => <div className="inspect-chunk" key={o.id}><div className="ic-head"><span>observation {o.id}</span></div><div className="ic-text">{o.text}</div></div>)}
    {(b.mental_models || []).map(m => <div className="inspect-chunk" key={m.id}><div className="ic-head"><span>mental model {m.id}</span></div><div className="ic-text">{m.text}</div></div>)}
    {(b.directives || []).map(d => <div className="inspect-chunk" key={d.id}><div className="ic-head"><span>directive</span></div><div className="ic-text">{d.text || d.content || d.name}</div></div>)}
    {r.trace?.tool_calls?.length ? <details className="banks-trace"><summary>Tool calls ({r.trace.tool_calls.length})</summary>
      <ol>{r.trace.tool_calls.map((c, i) => <li key={i}><b>{c.tool}</b>{c.iteration !== undefined ? ` (step ${c.iteration})` : ''}{c.duration_ms !== undefined ? ` · ${c.duration_ms} ms` : ''}
        <pre className="banks-pre">{JSON.stringify(c.input)}</pre></li>)}</ol></details> : null}
  </div>;
}
