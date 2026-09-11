import { useCallback, useEffect, useMemo, useState } from 'react';

const MONTHS = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];
const pad = (n: number) => String(n).padStart(2, '0');
const isoLocal = (date: Date) => `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;

export const todayDate = () => isoLocal(new Date());
export const shiftDate = (iso: string, delta: number) => {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(iso);
  const date = m ? new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 12) : new Date();
  date.setDate(date.getDate() + delta);
  return isoLocal(date);
};

export function CalendarPanel({ dates, close, openDay, onError }: {
  dates: () => Promise<string[]>;
  close: () => void;
  openDay: (date: string) => Promise<void>;
  onError: (message: string) => void;
}) {
  const now = new Date();
  const [month, setMonth] = useState(() => new Date(now.getFullYear(), now.getMonth(), 1));
  const [entries, setEntries] = useState<Set<string>>(new Set());
  const reload = useCallback(async () => { try { setEntries(new Set(await dates())); } catch (e) { onError(e instanceof Error ? e.message : 'Could not load daily notes'); } }, [dates, onError]);
  useEffect(() => { void reload(); }, [reload]);
  const cells = useMemo(() => {
    const first = month.getDay(), last = new Date(month.getFullYear(), month.getMonth() + 1, 0).getDate();
    return Array.from({ length: first + last }, (_, index) => index < first ? undefined : index - first + 1);
  }, [month]);
  const today = todayDate();
  const open = async (date: string) => { await openDay(date); close(); };
  return <div id="calendar-modal" className="modal" role="dialog" onMouseDown={e => e.currentTarget === e.target && close()}><div className="modal-box"><header className="modal-head"><span id="cal-title">{MONTHS[month.getMonth()]} {month.getFullYear()}</span><span><button id="cal-prev" className="icon" title="previous month" onClick={() => setMonth(current => new Date(current.getFullYear(), current.getMonth() - 1, 1))}>‹</button><button id="cal-next" className="icon" title="next month" onClick={() => setMonth(current => new Date(current.getFullYear(), current.getMonth() + 1, 1))}>›</button><button id="cal-close" className="icon" onClick={close}>✕</button></span></header><div id="calendar-body"><div className="cal-grid">{['S', 'M', 'T', 'W', 'T', 'F', 'S'].map((day, index) => <div className="cal-dow" key={`${day}${index}`}>{day}</div>)}{cells.map((day, index) => { if (!day) return <div className="cal-cell empty" key={`empty${index}`} />; const date = `${month.getFullYear()}-${pad(month.getMonth() + 1)}-${pad(day)}`; return <button type="button" className={`cal-cell${entries.has(date) ? ' has' : ''}${date === today ? ' today' : ''}`} data-d={date} key={date} onClick={() => void open(date)}>{day}</button>; })}</div></div></div></div>;
}
