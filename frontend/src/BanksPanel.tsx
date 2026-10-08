import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { ApiError } from './api';
import {
  createBanksApi, createFromTemplate, isModelRequired, UNAVAILABLE, type BankProfile, type BanksApi, type BankStats, type BankSummary,
  type Delivery, type Directive, type DuplicateCandidate, type DocumentDetail, type DocumentSummary, type EntityDetail, type EntitySummary, type Fact, type Maybe, type MentalModel,
  type ModelNode, type Observation, type Operation, type RecallResponse, type ReflectResponse, type Request, type Webhook,
} from './banksApi';
import {
  armNames, chunkId, CONFIG_KEYS, CONFIG_TEXT_KEYS, deliveryText, eventsLabel, factDate, filterFacts, fmtScore, isTerminal, OP_STATUSES, opKindLabel, parseTags,
  radialLayout, rankRows, staleText, validBankId, validWebhookUrl, WEBHOOK_EVENTS, type BankTemplate,
} from './banksModel';

// Memory banks: list, profile, memories, sources, entities, observations,
// mental models, directives, operations, and a recall/reflect playground.
// Every server call is in banksApi.ts. A server from before a feature existed
// is told apart from an error, and the tab says the feature is not there.

type Tab = 'profile' | 'memories' | 'documents' | 'entities' | 'observations' | 'duplicates' | 'models' | 'directives' | 'operations' | 'webhooks' | 'playground';
const TABS: [Tab, string][] = [
  ['playground', 'Playground'], ['memories', 'Memories'], ['documents', 'Documents'], ['entities', 'Entities'],
  ['observations', 'Observations'], ['duplicates', 'Duplicates'], ['models', 'Models'], ['directives', 'Directives'], ['operations', 'Operations'], ['webhooks', 'Webhooks'], ['profile', 'Profile'],
];

const NO_MODEL = 'This needs a language model, and none is configured on the server.';
const message = (e: unknown) => (isModelRequired(e) ? NO_MODEL : e instanceof Error ? e.message : String(e));
function ErrorText({ error }: { error: unknown }) {
  return error ? <p className={'vault-note banks-error' + (isModelRequired(error) ? ' banks-model-required' : '')} role="alert" data-code={error instanceof ApiError ? error.code : undefined}>{message(error)}</p> : null;
}
function Unavailable({ what }: { what: string }) {
  return <p className="vault-note banks-unavailable" data-testid="unavailable">{what} — not available on this server (it predates them).</p>;
}
function Badges({ f }: { f: { authority?: string; disputed_by?: string; doc_removed?: boolean; challenges?: string } }) {
  return <>
    {f.authority === 'human' && <span className="banks-badge human" title="A person wrote or corrected this">human</span>}
    {f.disputed_by && <span className="banks-badge disputed" title={`Contradicts ${f.disputed_by}`}>disputed</span>}
    {f.challenges && <span className="banks-badge disputed" title={`A model's revision of ${f.challenges}, filed for review`}>challenges {f.challenges}</span>}
    {f.doc_removed && <span className="banks-badge removed" title="Its source text no longer exists">source removed</span>}
  </>;
}
const when = (s?: string) => (s || '').slice(0, 16).replace('T', ' ');

/** Poll an operation until it finishes (or give up after `limitMs`, returning its last state). */
async function waitOperation(api: BanksApi, bank: string, id: string, limitMs = 180000): Promise<Operation> {
  const until = Date.now() + limitMs;
  for (let delay = 300; ; delay = Math.min(delay * 1.5, 2000)) {
    const op = await api.operation(bank, id);
    if (isTerminal(op.status) || Date.now() > until) return op;
    await new Promise(r => setTimeout(r, delay));
  }
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
        <StatsLine api={api} bank={bank} tab={tab} />
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
          {tab === 'duplicates' && <DuplicatesTab api={api} bank={bank} />}
          {tab === 'models' && <ModelsTab api={api} bank={bank} />}
          {tab === 'directives' && <DirectivesTab api={api} bank={bank} />}
          {tab === 'operations' && <OperationsTab api={api} bank={bank} />}
          {tab === 'webhooks' && <WebhooksTab api={api} bank={bank} />}
          {tab === 'playground' && <PlaygroundTab api={api} bank={bank} />}
        </div>
      </>}
    </div>
  </div>;
}

/** One line of counts under the bank name, refreshed when the tab changes. */
function StatsLine({ api, bank, tab }: { api: BanksApi; bank: string; tab: Tab }) {
  const [s, setS] = useState<Maybe<BankStats>>();
  useEffect(() => { void api.stats(bank).then(setS).catch(() => setS(undefined)); }, [api, bank, tab]);
  if (!s || s === UNAVAILABLE) return null;
  const running = (s.operations_by_status?.queued || 0) + (s.operations_by_status?.running || 0);
  return <p className="vault-note banks-stats" id="banks-stats">
    {s.facts} facts · {s.observations} observations · {s.mental_models} models · {s.documents} documents
    {s.pending_consolidation > 0 && <> · {s.pending_consolidation} awaiting consolidation</>}
    {running > 0 && <> · {running} operation(s) in progress</>}
    {!s.model_available && <> · <span className="banks-badge" title="Retain stores sentences or chunks, reflect quotes what recall finds, and consolidation and model refreshes are off">no language model</span></>}
  </p>;
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

function TemplatePreview({ t }: { t: BankTemplate & { builtin?: boolean } }) {
  const m = t.manifest;
  const config = Object.entries(m.bank?.config || {});
  return <div className="banks-template" data-testid="template-preview">
    <p className="vault-note">{t.description}</p>
    {m.mental_models?.length ? <><div className="pr-clabel">Mental models</div>
      <ul className="banks-facts">{m.mental_models.map(x => <li key={x.id}><b>{x.name}</b> — {x.question}</li>)}</ul></> : null}
    {m.directives?.length ? <><div className="pr-clabel">Directives</div>
      <ul className="banks-facts">{m.directives.map(d => <li key={d.name || d.text}>{d.name ? <b>{d.name}: </b> : null}{d.text}</li>)}</ul></> : null}
    {config.length ? <p className="vault-note">Settings: {config.map(([k, v]) => `${k}=${v}`).join(', ')}</p> : null}
    {t.builtin && <p className="vault-note">This server has no template catalogue; only the profile settings apply.</p>}
  </div>;
}

function CreateBank({ api, created }: { api: BanksApi; created: (id: string) => void }) {
  const [templates, setTemplates] = useState<(BankTemplate & { builtin?: boolean })[]>([]);
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
    {chosen && <TemplatePreview t={chosen} />}
    <label>Mission <textarea name="mission" rows={2} value={mission} onChange={e => setMission(e.target.value)} placeholder={chosen?.manifest.bank?.mission || 'What this bank is for'} /></label>
    <ErrorText error={error} />
    <button type="submit" className="btn" disabled={busy}>Create bank</button>
  </form>;
}

// ---- profile -----------------------------------------------------------------

function ProfileTab({ api, bank, deleted }: { api: BanksApi; bank: string; deleted: () => void }) {
  const [p, setP] = useState<BankProfile>(), [error, setError] = useState<unknown>(), [saved, setSaved] = useState('');
  useEffect(() => { void api.profile(bank).then(setP).catch(setError); }, [api, bank]);
  if (!p) return <><ErrorText error={error} /><p className="vault-note">Loading…</p></>;
  const edit = (patch: Partial<BankProfile>) => { setP({ ...p, ...patch }); setSaved(''); };
  const save = async () => {
    setError(undefined);
    try {
      // Directives have their own tab and routes; the profile leaves them be.
      setP(await api.update(bank, { name: p.name, mission: p.mission, retain_mission: p.retain_mission, disposition: p.disposition, config: p.config }));
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
    <div className="pr-clabel">Settings</div>
    <div className="banks-config">
      {CONFIG_KEYS.map(k => <label key={k.key}>{k.label}
        <select name={k.key} value={p.config[k.key] || ''} onChange={e => setConfig(k.key, e.target.value)}>
          <option value="">default</option>{k.values.map(v => <option key={v} value={v}>{v}</option>)}
        </select></label>)}
      {CONFIG_TEXT_KEYS.map(k => <label key={k.key}>{k.label}
        <input name={k.key} value={p.config[k.key] || ''} placeholder={k.placeholder} onChange={e => setConfig(k.key, e.target.value)} /></label>)}
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
  const [background, setBackground] = useState(false);
  const [status, setStatus] = useState(''), [error, setError] = useState<unknown>(), [busy, setBusy] = useState(false);
  const submit = async () => {
    setBusy(true); setError(undefined); setStatus('');
    try {
      const r = await api.retain(bank, [{ content, document_id: doc || undefined, context: context || undefined, tags: tags ? parseTags(tags) : undefined }], { mode, async: background });
      setStatus(r.async ? `Queued: operation ${r.operation_id}. See Operations.`
        : `Retained: ${(r.documents || []).map(d => `${d.document_id} (${d.facts} facts${d.unchanged ? ', unchanged' : ''})`).join(', ')}`);
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
      <label className="chk"><input type="checkbox" name="async" checked={background} onChange={e => setBackground(e.target.checked)} />in the background</label>
    </div>
    <ErrorText error={error} /><p className="vault-note" id="banks-retain-status">{status}</p>
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

function ObservationCard({ o, children }: { o: Observation; children?: ReactNode }) {
  const quotes = (o.evidence || []).filter(e => e.quote);
  return <div className={'inspect-chunk' + (o.challenges ? ' banks-disputed' : '')} data-observation={o.id}>
    <div className="ic-head"><span>{o.proof_count} supporting fact(s) <Badges f={o} /></span><span className="ic-score">{when(o.updated_at || o.superseded_at)}</span></div>
    <div className="ic-text">{o.text}</div>
    <div className="banks-sub">{o.id}{o.tags?.length ? ` · tags: ${o.tags.join(', ')}` : ''}</div>
    {(quotes.length > 0 || o.source_fact_ids.length > 0) && <details className="banks-evidence"><summary>Evidence</summary>
      <ul className="banks-facts">{quotes.length ? quotes.map(e => <li key={e.fact_id + e.quote}><code>{e.fact_id}</code> “{e.quote}”</li>)
        : o.source_fact_ids.map(id => <li key={id}><code>{id}</code></li>)}</ul></details>}
    {children}
  </div>;
}

function ObservationsTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [data, setData] = useState<Maybe<{ items: Observation[]; total: number; history?: Observation[] }>>();
  const [q, setQ] = useState(''), [authority, setAuthority] = useState(''), [history, setHistory] = useState(false);
  const [error, setError] = useState<unknown>(), [note, setNote] = useState('');
  const load = useCallback(() => api.observations(bank, { q, authority, include_history: history, limit: 200 }).then(setData).catch(setError), [api, bank, q, authority, history]);
  useEffect(() => { void load(); }, [load]);
  if (data === UNAVAILABLE) return <Unavailable what="Observations (consolidated knowledge)" />;
  const consolidate = async () => {
    setError(undefined); setNote('');
    try {
      const r = await api.consolidate(bank);
      if (r !== UNAVAILABLE) setNote(r.deduplicated ? `A consolidation is already queued (operation ${r.operation_id}).` : `Consolidation queued (operation ${r.operation_id}). See Operations.`);
    } catch (e) { setError(e); }
  };
  const [editId, setEditId] = useState<string>(), [draft, setDraft] = useState('');
  const saveEdit = async (o: Observation) => {
    setError(undefined);
    try { await api.updateObservation(bank, o.id, draft); setEditId(undefined); setNote('Saved. This observation is now marked human and consolidation will not overwrite it.'); void load(); } catch (e) { setError(e); }
  };
  const retire = async (o: Observation) => {
    const human = o.authority === 'human';
    if (!confirm(human ? 'A person wrote this observation. Retire it anyway? It moves to the history.' : 'Retire this observation? It moves to the history.')) return;
    try { await api.deleteObservation(bank, o.id, human); void load(); } catch (e) { setError(e); }
  };
  return <>
    <div className="banks-filters"><input aria-label="Search observations" placeholder="Search" value={q} onChange={e => setQ(e.target.value)} />
      <select aria-label="Author" value={authority} onChange={e => setAuthority(e.target.value)}>
        <option value="">Anyone</option><option value="human">Written by a person</option></select>
      <label className="banks-date"><input type="checkbox" checked={history} onChange={e => setHistory(e.target.checked)} /> history</label>
      <button className="btn" id="banks-consolidate" onClick={() => void consolidate()}>Consolidate now</button></div>
    <ErrorText error={error} /><p className="vault-note" id="banks-observations-note">{note}</p>
    {!data ? <p className="vault-note">Loading…</p> : <div id="banks-observations">
      {!data.items.length ? <p className="vault-note">No observations yet. They are written by consolidation, which needs a language model.</p> :
        <p className="vault-note">{data.total} observation(s). Edit observations.md in the vault to correct one: a person's text is never overwritten by a model.</p>}
      {data.items.map(o => <ObservationCard key={o.id} o={o}>
        {editId === o.id ? <form className="banks-form" data-testid="observation-editor" onSubmit={e => { e.preventDefault(); void saveEdit(o); }}>
          <textarea name="text" aria-label="Observation text" rows={4} value={draft} onChange={e => setDraft(e.target.value)} />
          <div className="banks-actions"><button type="submit" className="btn banks-small" disabled={!draft.trim() || draft === o.text}>Save</button>
            <button type="button" className="btn banks-small" onClick={() => setEditId(undefined)}>Cancel</button></div>
        </form> : <div className="banks-actions">
          <button className="btn banks-small" data-edit-observation={o.id} onClick={() => { setEditId(o.id); setDraft(o.text); }}>Edit</button>
          <button className="btn banks-small" onClick={() => void retire(o)}>Retire</button></div>}
      </ObservationCard>)}
      {history && data.history?.length ? <><div className="pr-clabel">History</div>
        {data.history.map((o, i) => <div className="inspect-chunk banks-history" key={o.id + i}>
          <div className="ic-head"><span>{o.deleted ? 'retired' : 'replaced'} · was {o.of}</span><span className="ic-score">{when(o.superseded_at)}</span></div>
          <div className="ic-text"><s>{o.text}</s></div></div>)}</> : null}
    </div>}
  </>;
}

// ---- duplicates ------------------------------------------------------------------------

function DuplicatesTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [data, setData] = useState<Maybe<{ candidates: DuplicateCandidate[] }>>();
  const [type, setType] = useState(''), [error, setError] = useState<unknown>(), [note, setNote] = useState('');
  const [pending, setPending] = useState<DuplicateCandidate>();
  const load = useCallback(() => api.duplicates(bank, { type, limit: 50 }).then(setData).catch(setError), [api, bank, type]);
  useEffect(() => { void load(); }, [load]);
  if (data === UNAVAILABLE) return <Unavailable what="Duplicate review" />;
  const merge = async (c: DuplicateCandidate) => {
    setError(undefined); setNote('');
    try {
      const r = await api.mergeDuplicates(bank, c.keep.id, c.merge.id);
      setPending(undefined); setNote(`Merged ${r.merged} into ${r.kept}. The old text is struck through and kept in its file.`); void load();
    } catch (e) { setError(e); }
  };
  return <>
    <div className="banks-filters"><select aria-label="Kind" value={type} onChange={e => setType(e.target.value)}>
      <option value="">Facts and observations</option><option value="fact">Facts</option><option value="observation">Observations</option></select></div>
    <ErrorText error={error} /><p className="vault-note" id="banks-duplicates-note">{note}</p>
    {!data ? <p className="vault-note">Loading…</p> : !data.candidates.length ? <p className="vault-note">No near-duplicates found.</p> :
      <div id="banks-duplicates">{data.candidates.map(c => <div className="inspect-chunk" key={c.keep.id + c.merge.id} data-duplicate={`${c.keep.id}|${c.merge.id}`}>
        <div className="ic-head"><span>{c.type} · shares {c.shared.join(', ')}</span><span className="ic-score">{c.score.toFixed(2)}</span></div>
        <div className="banks-compare">
          <div><div className="pr-clabel">Keep {c.keep.human && <span className="banks-badge human">human</span>}</div><div className="ic-text">{c.keep.text}</div><div className="banks-sub">{c.keep.id}</div></div>
          <div><div className="pr-clabel">Merge into it {c.merge.human && <span className="banks-badge human">human</span>}</div><div className="ic-text">{c.merge.text}</div><div className="banks-sub">{c.merge.id}</div></div>
        </div>
        {pending === c ? <div className="banks-actions" data-testid="merge-confirm">
          <span className="vault-note">Strike the right-hand text through and keep it in the file?</span>
          <button className="btn banks-small" data-confirm-merge onClick={() => void merge(c)}>Confirm merge</button>
          <button className="btn banks-small" onClick={() => setPending(undefined)}>Cancel</button></div>
          : <div className="banks-actions"><button className="btn banks-small" data-merge onClick={() => setPending(c)}>Merge</button></div>}
      </div>)}</div>}
  </>;
}

// ---- mental models ------------------------------------------------------------------------

function ModelTree({ nodes, selected, select }: { nodes: ModelNode[]; selected?: string; select: (id: string) => void }) {
  return <ul className="banks-tree">{nodes.map(n => <li key={n.kind + n.path}>
    {n.kind === 'page' && n.model ? <a className={'wikilink' + (selected === n.model.id ? ' on' : '')} data-model={n.model.id} onClick={() => select(n.model!.id)}>
      {n.model.name || n.name}{n.model.pending_proposal && <span className="banks-badge proposal">proposal</span>}
      {n.model.is_stale && <span className="banks-badge stale" title={staleText(n.model.stale_reason)}>stale</span>}</a>
      : <span className="banks-folder">{n.name}/</span>}
    {n.children?.length ? <ModelTree nodes={n.children} selected={selected} select={select} /> : null}
  </li>)}</ul>;
}

function ModelView({ api, bank, model, changed, moved, deleted }: {
  api: BanksApi; bank: string; model: MentalModel; changed: (m?: MentalModel) => void; moved: (id: string) => void; deleted: () => void;
}) {
  const [editing, setEditing] = useState(false), [draft, setDraft] = useState(model.body || ''), [folder, setFolder] = useState(model.folder);
  const [mode, setMode] = useState<'full' | 'delta'>(model.refresh_mode === 'delta' ? 'delta' : 'full');
  const [error, setError] = useState<unknown>(), [note, setNote] = useState('');
  useEffect(() => { setDraft(model.body || ''); setFolder(model.folder); setEditing(false); }, [model.id, model.version, model.body, model.folder]);
  // fn may return the note to show in place of `done`.
  const act = async (fn: () => Promise<unknown>, done: string) => {
    setError(undefined); setNote('');
    try { const r = await fn(); setNote(typeof r === 'string' ? r : done); changed(); } catch (e) { setError(e); }
  };
  const p = model.pending_proposal;
  return <div id="banks-model" data-model={model.id}>
    <h3 className="banks-h3">{model.name} <Badges f={model} />{model.is_stale && <span className="banks-badge stale">stale</span>}</h3>
    <p className="vault-note">{model.question}</p>
    <p className="vault-note banks-sub" data-testid="model-meta">
      {model.id} · version {model.version} · refresh {model.refresh} · budget {model.budget}
      {model.last_refreshed ? ` · refreshed ${when(model.last_refreshed)}` : ''}{model.is_stale ? ` · ${staleText(model.stale_reason)}` : ''}
      {model.based_on?.length ? ` · based on ${model.based_on.length} memories` : ''}{model.tags?.length ? ` · tags ${model.tags.join(', ')}` : ''}
    </p>
    <div className="banks-actions">
      <select aria-label="Refresh mode" id="banks-model-refresh-mode" value={mode} onChange={e => setMode(e.target.value as 'full' | 'delta')}
        title="Full rewrites the answer; delta only folds in what changed since the last refresh">
        <option value="full">Full rewrite</option><option value="delta">Delta (changes only)</option></select>
      <button className="btn" id="banks-model-refresh" onClick={() => void act(async () => {
        const r = await api.refreshMentalModel(bank, model.id, mode);
        setNote(r.deduplicated ? `A refresh is already queued (operation ${r.operation_id}).` : `Refresh queued (operation ${r.operation_id}).`);
        // Follow the operation so the answer (or proposal) shows when it lands.
        const op = await waitOperation(api, bank, r.operation_id);
        const outcome = (op.result as { outcome?: string } | undefined)?.outcome;
        return op.status !== 'completed' ? `Refresh ${op.status}${op.error ? `: ${op.error}` : ''}.`
          : outcome === 'proposed' ? 'Refreshed: the new answer is filed as a proposal, because a person edited this one.'
          : outcome === 'no_sources' ? 'Refreshed: nothing in the bank answers this yet.'
          : `Refreshed (${outcome || 'done'}).`;
      }, '')}>Refresh</button>
      {!editing && <button className="btn" id="banks-model-edit" onClick={() => setEditing(true)}>Edit answer</button>}
      <button className="btn banks-danger" onClick={() => { if (confirm(`Delete ${model.name}?`)) void act(() => api.deleteMentalModel(bank, model.id), 'Deleted.').then(deleted); }}>Delete</button>
    </div>
    <ErrorText error={error} /><p className="vault-note" id="banks-model-note">{note}</p>
    {p ? <div className="banks-proposal" data-testid="proposal">
      <p className="vault-note">A refresh wrote a new answer, but a person has edited this one since the model last wrote it. Your text stays until you accept.</p>
      <div className="banks-compare"><div><div className="pr-clabel">Current</div><pre className="banks-pre" data-testid="proposal-current">{model.body}</pre></div>
        <div><div className="pr-clabel">Proposed {when(p.created_at)}</div><pre className="banks-pre" data-testid="proposal-content">{p.content}</pre></div></div>
      <div className="banks-actions">
        <button className="btn" id="banks-proposal-accept" onClick={() => void act(() => api.acceptProposal(bank, model.id), 'Proposal accepted; the model writes this answer again.')}>Accept proposal</button>
        <button className="btn" id="banks-proposal-reject" onClick={() => void act(() => api.rejectProposal(bank, model.id), 'Proposal rejected; your text stays.')}>Keep mine</button>
      </div>
    </div> : null}
    {editing ? <form className="banks-form" id="banks-model-editor" onSubmit={e => { e.preventDefault(); void act(() => api.updateMentalModel(bank, model.id, { body: draft }), 'Saved. Your text is now kept over model refreshes; a refresh files a proposal instead.'); }}>
      <textarea name="body" rows={10} value={draft} onChange={e => setDraft(e.target.value)} />
      <div className="banks-actions"><button type="submit" className="btn">Save answer</button><button type="button" className="btn" onClick={() => setEditing(false)}>Cancel</button></div>
    </form> : !p && <pre className="banks-pre" data-testid="model-body">{model.body || '(empty — refresh to write it, or edit it yourself)'}</pre>}
    <form className="banks-row-fields banks-move" onSubmit={e => { e.preventDefault(); void (async () => {
      setError(undefined);
      try { const m = await api.updateMentalModel(bank, model.id, { folder }); setNote(`Moved to ${m.id}.`); moved(m.id); } catch (err) { setError(err); }
    })(); }}>
      <input name="folder" aria-label="Folder" placeholder="folder (e.g. people)" value={folder} onChange={e => setFolder(e.target.value)} />
      <button type="submit" className="btn" disabled={folder === model.folder}>Move</button>
    </form>
  </div>;
}

function ModelsTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [tree, setTree] = useState<Maybe<{ roots: ModelNode[] }>>(), [error, setError] = useState<unknown>();
  const [selected, setSelected] = useState<string>(), [model, setModel] = useState<MentalModel>(), [note, setNote] = useState('');
  const [rev, setRev] = useState(0);
  const [name, setName] = useState(''), [mid, setMid] = useState(''), [folder, setFolder] = useState(''), [query, setQuery] = useState(''), [refresh, setRefresh] = useState('auto');
  const load = useCallback(() => api.modelTree(bank).then(setTree).catch(setError), [api, bank]);
  const loadModel = useCallback(() => {
    if (!selected) return setModel(undefined);
    void api.mentalModel(bank, selected).then(setModel).catch(e => { setModel(undefined); setError(e); });
  }, [api, bank, selected, rev]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => { loadModel(); }, [loadModel]);
  if (tree === UNAVAILABLE) return <Unavailable what="Mental models" />;
  const create = async () => {
    setError(undefined); setNote('');
    try {
      const r = await api.createMentalModel(bank, { id: mid || undefined, name, question: query, folder: folder || undefined, refresh });
      if (r === UNAVAILABLE) return;
      setNote(r.operation_id ? `Created ${r.mental_model_id}; its first answer is being written (operation ${r.operation_id}).`
        : `Created ${r.mental_model_id}. No language model is configured, so its answer stays empty until you write one.`);
      setName(''); setMid(''); setQuery(''); setFolder('');
      setSelected(r.mental_model_id); void load();
      if (r.operation_id) {
        const op = await waitOperation(api, bank, r.operation_id);
        setNote(op.status === 'completed' ? `Created ${r.mental_model_id}; its first answer is written.` : `Created ${r.mental_model_id}; writing its answer ${op.status}${op.error ? `: ${op.error}` : ''}.`);
        setRev(n => n + 1); void load();
      }
    } catch (e) { setError(e); }
  };
  const roots = tree?.roots || [];
  return <>
    <ErrorText error={error} /><p className="vault-note" id="banks-models-note">{note}</p>
    {!tree ? <p className="vault-note">Loading…</p> : <div className="banks-split">
      <div className="banks-tree-wrap" id="banks-model-tree">{roots.length ? <ModelTree nodes={roots} selected={selected} select={setSelected} /> : <p className="vault-note">No mental models yet.</p>}
        <form className="banks-form" id="banks-model-create" onSubmit={e => { e.preventDefault(); void create(); }}>
          <div className="pr-clabel">New mental model</div>
          <input name="name" placeholder="Name" value={name} onChange={e => setName(e.target.value)} required />
          <textarea name="question" rows={2} placeholder="The question it answers" value={query} onChange={e => setQuery(e.target.value)} required />
          <div className="banks-row-fields">
            <input name="id" placeholder="id (optional)" value={mid} onChange={e => setMid(e.target.value)} />
            <input name="folder" placeholder="folder (optional)" value={folder} onChange={e => setFolder(e.target.value)} />
            <select name="refresh" aria-label="Refresh" value={refresh} onChange={e => setRefresh(e.target.value)}>
              <option value="auto">refresh after consolidation</option><option value="manual">refresh by hand</option></select>
          </div>
          <button type="submit" className="btn">Create</button>
        </form>
      </div>
      <div className="banks-model">{model ? <ModelView api={api} bank={bank} model={model}
        changed={() => { loadModel(); void load(); }}
        moved={id => { setSelected(id); void load(); }}
        deleted={() => { setSelected(undefined); void load(); }} />
        : <p className="vault-note">Choose a model to view it.</p>}</div>
    </div>}
  </>;
}

// ---- directives ------------------------------------------------------------------------------

function DirectivesTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [data, setData] = useState<Maybe<{ items: Directive[]; total: number }>>(), [error, setError] = useState<unknown>();
  const [text, setText] = useState(''), [name, setName] = useState(''), [tags, setTags] = useState(''), [priority, setPriority] = useState('');
  const [editing, setEditing] = useState<string>(), [draft, setDraft] = useState('');
  const load = useCallback(() => api.directives(bank).then(setData).catch(setError), [api, bank]);
  useEffect(() => { void load(); }, [load]);
  if (data === UNAVAILABLE) return <Unavailable what="Directive routes" />;
  const act = async (fn: () => Promise<unknown>) => { setError(undefined); try { await fn(); void load(); } catch (e) { setError(e); } };
  return <>
    <p className="vault-note">Standing rules every reflect answer is checked against. Tagged directives apply only to reflects with matching tags.</p>
    <ErrorText error={error} />
    {!data ? <p className="vault-note">Loading…</p> : !data.items.length ? <p className="vault-note">No directives.</p> :
      <div className="banks-scroll"><table className="usage-table banks-table" id="banks-directives">
        <thead><tr><th>directive</th><th>tags</th><th className="num">priority</th><th>active</th><th /></tr></thead>
        <tbody>{data.items.map(d => <tr key={d.id} data-directive={d.id} className={d.inactive ? 'banks-disputed' : ''}>
          <td className="banks-fact-text">{editing === d.id
            ? <input aria-label="Directive text" value={draft} onChange={e => setDraft(e.target.value)} />
            : <>{d.name && <b>{d.name}: </b>}{d.text}</>}<div className="banks-sub">{d.id}</div></td>
          <td>{(d.tags || []).join(', ')}</td><td className="num">{d.priority ?? 0}</td>
          <td><input type="checkbox" aria-label="Active" checked={!d.inactive} onChange={e => void act(() => api.updateDirective(bank, d.id, { is_active: e.target.checked }))} /></td>
          <td className="banks-actions">{editing === d.id
            ? <><button className="btn banks-small" onClick={() => void act(() => api.updateDirective(bank, d.id, { text: draft })).then(() => setEditing(undefined))}>Save</button>
              <button className="btn banks-small" onClick={() => setEditing(undefined)}>Cancel</button></>
            : <button className="btn banks-small" onClick={() => { setEditing(d.id); setDraft(d.text); }}>Edit</button>}
            <button className="btn banks-small banks-danger" onClick={() => { if (confirm('Delete this directive?')) void act(() => api.deleteDirective(bank, d.id)); }}>Delete</button></td>
        </tr>)}</tbody>
      </table></div>}
    <form className="banks-form" id="banks-directive-create" onSubmit={e => { e.preventDefault(); void act(async () => {
      await api.createDirective(bank, { text: text.trim(), name: name.trim() || undefined, tags: tags ? parseTags(tags) : undefined, priority: priority ? Number(priority) : undefined });
      setText(''); setName(''); setTags(''); setPriority('');
    }); }}>
      <div className="pr-clabel">New directive</div>
      <input id="banks-new-directive" name="text" placeholder="A standing rule, e.g. Never reveal account numbers" value={text} onChange={e => setText(e.target.value)} required />
      <div className="banks-row-fields">
        <input name="name" placeholder="name (optional, unique)" value={name} onChange={e => setName(e.target.value)} />
        <input name="tags" placeholder="tags, comma separated" value={tags} onChange={e => setTags(e.target.value)} />
        <input name="priority" type="number" placeholder="priority" value={priority} onChange={e => setPriority(e.target.value)} />
      </div>
      <button type="submit" className="btn" disabled={!text.trim()}>Add directive</button>
    </form>
  </>;
}

// ---- operations ------------------------------------------------------------------------------

function OperationsTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [data, setData] = useState<Maybe<{ operations: Operation[]; total: number }>>(), [status, setStatus] = useState(''), [type, setType] = useState('');
  const [error, setError] = useState<unknown>(), [open, setOpen] = useState<Operation>();
  const load = useCallback(() => api.operations(bank, { status, type, limit: 50 }).then(setData).catch(setError), [api, bank, status, type]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => {
    if (!data || data === UNAVAILABLE || data.operations.every(o => isTerminal(o.status))) return;
    const timer = setInterval(() => void load(), 2000);
    return () => clearInterval(timer);
  }, [data, load]);
  if (data === UNAVAILABLE) return <Unavailable what="Background operations" />;
  const show = (o: Operation) => void api.operation(bank, o.id).then(setOpen).catch(setError);
  return <>
    <div className="banks-filters">
      <select aria-label="Status" value={status} onChange={e => setStatus(e.target.value)}>
        <option value="">All statuses</option>{OP_STATUSES.map(s => <option key={s}>{s}</option>)}</select>
      <select aria-label="Kind" value={type} onChange={e => setType(e.target.value)}>
        <option value="">All kinds</option>{['retain', 'consolidation', 'refresh_mental_model'].map(k => <option key={k} value={k}>{opKindLabel(k)}</option>)}</select>
      <button className="btn" onClick={() => void load()}>Reload</button></div>
    <ErrorText error={error} />
    {!data ? <p className="vault-note">Loading…</p> : !data.operations.length ? <p className="vault-note">No operations.</p> : <>
      <p className="vault-note">{data.total} operation(s), newest first.</p>
      <div className="banks-scroll"><table className="usage-table banks-table" id="banks-operations">
        <thead><tr><th>operation</th><th>status</th><th>progress</th><th>created</th><th>finished</th><th /></tr></thead>
        <tbody>{data.operations.map(o => <tr key={o.id} data-operation={o.id} data-kind={o.kind} data-status={o.status}>
          <td><a className="wikilink" onClick={() => show(o)}>{opKindLabel(o.kind || o.type)}</a><div className="banks-sub">{o.id}{o.attempts > 1 ? ` · attempt ${o.attempts}` : ''}</div>
            {o.error && <div className="banks-sub banks-error">{o.error}</div>}</td>
          <td><span className={`banks-chip ${o.status}`}>{o.status}{o.cancel_requested && !isTerminal(o.status) ? ' (cancelling)' : ''}</span></td>
          <td>{o.progress || ''}</td>
          <td>{when(o.created_at)}</td><td>{when(o.finished_at)}</td>
          <td>{!isTerminal(o.status) && <button className="btn banks-small" onClick={() => void api.cancelOperation(bank, o.id).then(load).catch(setError)}>Cancel</button>}</td>
        </tr>)}</tbody>
      </table></div></>}
    {open && <div className="inspect-chunk" id="banks-operation">
      <div className="ic-head"><span>{opKindLabel(open.kind)} · {open.status}</span><button className="btn banks-small" onClick={() => setOpen(undefined)}>Close</button></div>
      {open.result !== undefined && <><div className="pr-clabel">Result</div><pre className="banks-pre">{JSON.stringify(open.result, null, 2)}</pre></>}
      {open.error && <p className="banks-error">{open.error}</p>}
    </div>}
  </>;
}

// ---- webhooks -----------------------------------------------------------------------------------

function WebhooksTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [data, setData] = useState<Maybe<{ items: Webhook[]; total: number }>>(), [error, setError] = useState<unknown>();
  const [url, setUrl] = useState(''), [secret, setSecret] = useState(''), [events, setEvents] = useState<string[]>([]);
  const [made, setMade] = useState<Webhook>(), [open, setOpen] = useState<string>(), [deliveries, setDeliveries] = useState<Delivery[]>();
  const load = useCallback(() => api.webhooks(bank).then(setData).catch(setError), [api, bank]);
  useEffect(() => { void load(); }, [load]);
  const showDeliveries = useCallback((id: string) => {
    setOpen(id); setDeliveries(undefined);
    api.deliveries(bank, id).then(r => setDeliveries(r.items)).catch(setError);
  }, [api, bank]);
  if (data === UNAVAILABLE) return <Unavailable what="Webhook routes" />;
  const act = async (fn: () => Promise<unknown>) => { setError(undefined); try { await fn(); void load(); } catch (e) { setError(e); } };
  return <>
    <p className="vault-note">Post a signed JSON event to a URL when this bank finishes a retain, a consolidation or a reflect. Failed deliveries are retried.</p>
    <ErrorText error={error} />
    {!data ? <p className="vault-note">Loading…</p> : !data.items.length ? <p className="vault-note">No webhooks.</p> :
      <div className="banks-scroll"><table className="usage-table banks-table" id="banks-webhooks">
        <thead><tr><th>webhook</th><th>events</th><th>on</th><th /></tr></thead>
        <tbody>{data.items.map(w => <tr key={w.id} data-webhook={w.id} className={w.enabled ? '' : 'banks-disputed'}>
          <td className="banks-fact-text">{w.url}<div className="banks-sub">{w.id}{w.has_secret ? ' · signed' : ''}{w.bank_id ? '' : ' · every bank'}</div></td>
          <td>{eventsLabel(w.events)}</td>
          <td><input type="checkbox" aria-label="Enabled" checked={w.enabled} onChange={e => void act(() => api.updateWebhook(bank, w.id, { enabled: e.target.checked }))} /></td>
          <td className="banks-actions">
            <button className="btn banks-small" onClick={() => (open === w.id ? setOpen(undefined) : showDeliveries(w.id))}>{open === w.id ? 'Hide deliveries' : 'Deliveries'}</button>
            <button className="btn banks-small banks-danger" onClick={() => { if (confirm('Delete this webhook?')) void act(() => api.deleteWebhook(bank, w.id)); }}>Delete</button></td>
        </tr>)}</tbody>
      </table></div>}
    {open && <div className="inspect-chunk" id="banks-deliveries">
      <div className="ic-head"><span>Recent deliveries · {open}</span><button className="btn banks-small" onClick={() => showDeliveries(open)}>Reload</button></div>
      {!deliveries ? <p className="vault-note">Loading…</p> : !deliveries.length ? <p className="vault-note">Nothing has been sent yet.</p> :
        <table className="usage-table banks-table"><thead><tr><th>event</th><th>result</th><th>when</th></tr></thead>
          <tbody>{deliveries.map(d => <tr key={d.id} data-delivery={d.id} data-status={d.status}>
            <td>{d.event}</td><td><span className={`banks-chip ${d.status === 'delivered' ? 'completed' : d.status === 'failed' ? 'failed' : 'running'}`}>{deliveryText(d)}</span></td>
            <td>{when(d.updated_at || d.created_at)}</td></tr>)}</tbody></table>}
    </div>}
    {made?.secret && <p className="vault-note" id="banks-webhook-secret">Signing secret for {made.url}, shown once: <code>{made.secret}</code></p>}
    <form className="banks-form" id="banks-webhook-create" onSubmit={e => { e.preventDefault(); void act(async () => {
      const w = await api.createWebhook(bank, { url: url.trim(), events: events.length ? events : undefined, secret: secret.trim() || undefined });
      setMade(w); setUrl(''); setSecret(''); setEvents([]);
    }); }}>
      <div className="pr-clabel">New webhook</div>
      <input id="banks-new-webhook" name="url" type="url" placeholder="https://example.com/hook" value={url} onChange={e => setUrl(e.target.value)} required />
      <div className="banks-row-fields">
        {WEBHOOK_EVENTS.map(ev => <label key={ev}><input type="checkbox" checked={events.includes(ev)}
          onChange={e => setEvents(e.target.checked ? [...events, ev] : events.filter(x => x !== ev))} /> {ev}</label>)}
      </div>
      <input name="secret" placeholder="signing secret (optional)" value={secret} onChange={e => setSecret(e.target.value)} />
      <button type="submit" className="btn" disabled={!validWebhookUrl(url)}>Add webhook</button>
    </form>
  </>;
}

// ---- playground ---------------------------------------------------------------------------------

function PlaygroundTab({ api, bank }: { api: BanksApi; bank: string }) {
  const [query, setQuery] = useState(''), [budget, setBudget] = useState('mid'), [maxTokens, setMaxTokens] = useState(4096);
  const [types, setTypes] = useState<string[]>(['world', 'experience', 'observation']), [tags, setTags] = useState(''), [showTools, setShowTools] = useState(false);
  const [recall, setRecall] = useState<RecallResponse>(), [reflect, setReflect] = useState<Maybe<ReflectResponse>>();
  const [error, setError] = useState<unknown>(), [busy, setBusy] = useState('');
  const run = async (kind: 'recall' | 'reflect') => {
    setBusy(kind); setError(undefined);
    const tagList = tags ? parseTags(tags) : undefined;
    try {
      if (kind === 'recall') { setReflect(undefined); setRecall(await api.recall(bank, { query, budget, max_tokens: maxTokens, types, tags: tagList, trace: true })); }
      // Reflect's budget is turns of the loop; its max_tokens is the answer's length target.
      else { setRecall(undefined); setReflect(await api.reflect(bank, { query, budget, fact_types: types, tags: tagList, trace: showTools })); }
    } catch (e) { setError(e); } finally { setBusy(''); }
  };
  return <>
    <form className="banks-form" id="banks-playground" onSubmit={e => { e.preventDefault(); void run('recall'); }}>
      <textarea id="banks-query" name="query" rows={2} value={query} onChange={e => setQuery(e.target.value)} placeholder="Ask the bank something…" required />
      <div className="banks-row-fields">
        <label>Budget <select name="budget" value={budget} onChange={e => setBudget(e.target.value)}><option>low</option><option>mid</option><option>high</option></select></label>
        <label>Max tokens (recall) <input name="max_tokens" type="number" min={0} max={65536} value={maxTokens} onChange={e => setMaxTokens(Number(e.target.value))} /></label>
        <input name="tags" placeholder="tags" value={tags} onChange={e => setTags(e.target.value)} />
      </div>
      <div className="banks-row-fields">{['world', 'experience', 'observation'].map(t => <label key={t} className="chk">
        <input type="checkbox" checked={types.includes(t)} onChange={e => setTypes(e.target.checked ? [...types, t] : types.filter(x => x !== t))} />{t}</label>)}
        <label className="chk"><input type="checkbox" name="tool_calls" checked={showTools} onChange={e => setShowTools(e.target.checked)} />reflect tool calls</label></div>
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
  const b = r.based_on || { memories: [], observations: [], mental_models: [], directives: [] };
  const cited = (b.memories?.length || 0) + (b.observations?.length || 0) + (b.mental_models?.length || 0);
  return <div id="banks-reflect-results" data-mode={r.mode}>
    <div className="pr-clabel">Answer {r.mode === 'extractive' && <span className="banks-badge" title="No language model is configured: the answer quotes what recall found">extractive</span>}</div>
    <div className="banks-answer" data-testid="reflect-answer">{r.text}</div>
    <p className="vault-note">{[r.usage?.total_tokens ? `${r.usage.input_tokens ?? 0} tokens in · ${r.usage.output_tokens ?? 0} out` : '',
      r.iterations ? `${r.iterations} step(s)` : '', r.directives_checked ? 'checked against directives' : ''].filter(Boolean).join(' · ')}</p>
    <div className="pr-clabel">Based on ({cited})</div>
    <div id="banks-citations">
      {(b.mental_models || []).map(m => <div className="inspect-chunk" key={'m' + m.id} data-cite={m.id} data-kind="mental_model"><div className="ic-head"><span>mental model <code>{m.id}</code> {m.name}</span></div><div className="ic-text">{m.text}</div></div>)}
      {(b.observations || []).map(o => <div className="inspect-chunk" key={'o' + o.id} data-cite={o.id} data-kind="observation"><div className="ic-head"><span>observation <code>{o.id}</code> <Badges f={o} /></span><span className="ic-score">{o.proof_count ? `${o.proof_count} fact(s)` : ''}</span></div><div className="ic-text">{o.text}</div></div>)}
      {(b.memories || []).map(f => <div className="inspect-chunk" key={'f' + f.id} data-cite={f.id} data-kind="memory"><div className="ic-head"><span>{f.type || 'fact'} <code>{f.id}</code> <Badges f={f} /></span><span className="ic-score">{factDate(f)}</span></div><div className="ic-text">{f.text}</div></div>)}
    </div>
    {b.directives?.length ? <><div className="pr-clabel">Directives applied</div>
      <ul className="banks-facts" id="banks-reflect-directives">{b.directives.map(d => <li key={d.id}>{d.name ? <b>{d.name}: </b> : null}{d.text}</li>)}</ul></> : null}
    {r.structured_output !== undefined && r.structured_output !== null && <><div className="pr-clabel">Structured output</div><pre className="banks-pre">{JSON.stringify(r.structured_output, null, 2)}</pre></>}
    {r.structured_output_error && <p className="vault-note banks-error">{r.structured_output_error}</p>}
    {r.trace?.rejected_citations?.length ? <p className="vault-note">Dropped citations nothing had retrieved: {r.trace.rejected_citations.join(', ')}</p> : null}
    {r.trace?.tool_calls?.length ? <details className="banks-trace" id="banks-reflect-trace"><summary>Tool calls ({r.trace.tool_calls.length}){r.trace.levels?.length ? ` · levels ${r.trace.levels.join(' → ')}` : ''}</summary>
      <ol>{r.trace.tool_calls.map((c, i) => <li key={i}><b>{c.tool}</b>{c.iteration !== undefined ? ` (step ${c.iteration + 1})` : ''}{c.forced ? ' · run by the engine' : ''}{c.duration_ms !== undefined ? ` · ${c.duration_ms.toFixed(1)} ms` : ''}
        {c.error && <span className="banks-error"> · {c.error}</span>}
        <pre className="banks-pre">{JSON.stringify(c.input)}</pre></li>)}</ol></details> : null}
  </div>;
}
