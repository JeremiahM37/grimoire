import { useCallback, useEffect, useMemo, useState } from 'react';
import type { Note, Task } from './types';

type TasksApi = {
  tasks: (includeDone?: boolean) => Promise<Task[]>;
  note: (path: string) => Promise<Note>;
  update: (note: Note) => Promise<Note>;
};

export function TasksPanel({ api, close, open, activePath, onError }: {
  api: TasksApi;
  close: () => void;
  open: (path: string) => Promise<void>;
  activePath?: string;
  onError: (message: string) => void;
}) {
  const [includeDone, setIncludeDone] = useState(false);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [busy, setBusy] = useState(false);
  const reload = useCallback(async () => {
    try { setTasks(await api.tasks(includeDone)); } catch (e) { onError(e instanceof Error ? e.message : 'Could not load tasks'); }
  }, [api, includeDone, onError]);
  useEffect(() => { void reload(); }, [reload]);
  const groups = useMemo(() => {
    const byPath = new Map<string, { title: string; tasks: Task[] }>();
    for (const task of tasks) {
      const group = byPath.get(task.path) || { title: task.title || task.path, tasks: [] };
      group.tasks.push(task); byPath.set(task.path, group);
    }
    return [...byPath.entries()];
  }, [tasks]);
  const toggle = async (task: Task, done: boolean) => {
    // Keep the native checkbox controlled through its click.  Waiting for the
    // write first causes browsers to undo the click before the async response.
    setTasks(current => current.map(row => row.path === task.path && row.line === task.line ? { ...row, done } : row));
    setBusy(true);
    try {
      const note = await api.note(task.path);
      if (note.locked) throw new Error('Note is locked');
      const body = note.body.split('\n');
      if (task.line < 0 || task.line >= body.length) throw new Error('Task no longer exists at that line');
      const changed = body[task.line]!.replace(/^(\s*[-*]\s+)\[[ xX]\]/, (_, prefix: string) => `${prefix}[${done ? 'x' : ' '}]`);
      if (changed === body[task.line]) throw new Error('Task no longer exists at that line');
      await api.update({ ...note, body: body.map((line, index) => index === task.line ? changed : line).join('\n') });
      if (task.path === activePath) await open(task.path);
      await reload();
    } catch (e) { onError(e instanceof Error ? e.message : 'Could not update task'); await reload(); }
    finally { setBusy(false); }
  };
  const jump = async (path: string) => { close(); await open(path); };
  const openCount = tasks.filter(task => !task.done).length;
  return <div id="tasks-modal" className="modal" role="dialog" onMouseDown={e => e.currentTarget === e.target && close()}><div className="modal-box"><header className="modal-head"><span>☑ Tasks <span id="tasks-count" className="graph-stat">{openCount} open</span></span><span><label className="chk"><input id="tasks-done" type="checkbox" checked={includeDone} onChange={e => setIncludeDone(e.target.checked)} /> show done</label><button id="tasks-close" className="icon" onClick={close}>✕</button></span></header><div id="tasks-body">{!tasks.length ? <p className="vault-note">No tasks yet. Add <code>- [ ] a todo</code> to any note.</p> : groups.map(([path, group]) => <div className="task-group" key={path}><button type="button" className="tg-head" onClick={() => void jump(path)}>{group.title}</button>{group.tasks.map(task => <label className={'tg-item' + (task.done ? ' done' : '')} key={`${task.path}:${task.line}`}><input className="tg-box" type="checkbox" checked={task.done} disabled={busy} onChange={e => void toggle(task, e.target.checked)} /><span className="tg-text" onClick={() => void jump(path)}>{task.text}</span></label>)}</div>)}</div></div></div>;
}
