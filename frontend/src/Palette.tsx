import { useEffect, useMemo, useRef, useState } from 'react';
import { BASE_COMMANDS, buildPalette, flattenRows, markRuns, type PaletteNote, type PaletteRow } from './paletteModel';

export type PaletteProps = {
  /** Text the palette opens with (Ctrl+P passes '>'). */
  initial?: string;
  notes: PaletteNote[];
  templates: { name: string }[];
  pluginCommands: string[];
  recents: string[];
  command: (value: string) => void | Promise<void>;
  open: (path: string) => void | Promise<void>;
  create: (title: string) => void | Promise<void>;
  close: () => void;
};

function Highlighted({ label, marks }: { label: string; marks: number[] }) {
  return <>{markRuns(label, marks).map((run, index) => run.marked ? <mark key={index}>{run.text}</mark> : <span key={index}>{run.text}</span>)}</>;
}

/** Ctrl/⌘K, O and P palette. Rows carry their data in data-kind / data-value; actions read those, never textContent. */
export function Palette({ initial = '', notes, templates, pluginCommands, recents, command, open, create, close }: PaletteProps) {
  const [query, setQuery] = useState(initial);
  const [selected, setSelected] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);
  const commands = useMemo(() => [...BASE_COMMANDS, ...pluginCommands, ...templates.map(t => `New from: ${t.name}`)], [pluginCommands, templates]);
  const sections = useMemo(() => buildPalette({ query, notes, commands, recents }), [query, notes, commands, recents]);
  const rows = flattenRows(sections);
  const index = Math.min(selected, Math.max(0, rows.length - 1));

  useEffect(() => {
    listRef.current?.querySelector<HTMLElement>('.pal-item.sel')?.scrollIntoView({ block: 'nearest' });
  }, [index, query]);

  const run = (row: PaletteRow | undefined) => {
    if (!row) return;
    if (row.kind === 'create') { close(); void create(row.value); return; }
    if (row.kind === 'note') { close(); void open(row.value); return; }
    close();
    void command(row.value);
  };

  let flat = 0;
  return <div id="palette" className="modal" role="dialog" aria-label="Command palette" onMouseDown={event => event.currentTarget === event.target && close()}>
    <div className="modal-box palette-box">
      <input id="palette-input" autoFocus value={query} placeholder="Jump to a note or run a command…" aria-label="Jump to a note or run a command"
        onChange={event => { setQuery(event.target.value); setSelected(0); }}
        onKeyDown={event => {
          if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault();
            if (!rows.length) return;
            setSelected(current => (Math.min(current, rows.length - 1) + (event.key === 'ArrowDown' ? 1 : -1) + rows.length) % rows.length);
          } else if (event.key === 'Enter') {
            event.preventDefault();
            const element = listRef.current?.querySelector<HTMLElement>('.pal-item.sel');
            const kind = element?.dataset.kind, value = element?.dataset.value;
            run(rows.find(row => row.kind === kind && row.value === value) ?? rows[index]);
          } else if (event.key === 'Escape') {
            event.preventDefault();
            close();
          }
        }} />
      <div id="palette-list" ref={listRef}>
        {sections.map(section => <section key={section.title || 'results'} className="pal-section">
          {section.title && <h3 className="pal-heading">{section.title}</h3>}
          {section.rows.map(row => {
            const position = flat++;
            return <button type="button" key={`${row.kind}:${row.value}`} data-kind={row.kind} data-value={row.value}
              className={'pal-item' + (position === index ? ' sel' : '')}
              onMouseEnter={() => setSelected(position)} onClick={() => run(row)}>
              <span className="pal-label"><Highlighted label={row.label} marks={row.marks} /></span>
              {row.detail && <span className="pal-detail">{row.detail}</span>}
              {row.shortcut && <kbd className="pal-kbd">{row.shortcut}</kbd>}
            </button>;
          })}
        </section>)}
        {!rows.length && <p className="pal-empty">No matching notes or commands.</p>}
      </div>
    </div>
  </div>;
}

