// Android's back gesture / button walks the browser history.  A phone UI has
// "screens" (the note, a sheet) that are not URLs, so each one registers an
// entry here: back closes the topmost screen instead of leaving the app.
type Entry = { id: number; back: () => void };
const stack: Entry[] = [];
let counter = 0;
const depth = () => (history.state && typeof history.state.gm === 'number' ? history.state.gm : 0);

export function pushBack(back: () => void): number {
  const id = ++counter;
  stack.push({ id, back });
  const state = { ...(history.state || {}), gm: stack.length };
  // A stale entry from a screen that closed a moment ago is reused, not stacked.
  if (depth() >= stack.length) history.replaceState(state, '');
  else history.pushState(state, '');
  return id;
}

export function popBack(id: number): void {
  const index = stack.findIndex(entry => entry.id === id);
  if (index < 0) return;
  stack.splice(index, 1);
  // Wait one tick: a screen replacing another pushes synchronously and reuses the entry.
  setTimeout(() => { const extra = depth() - stack.length; if (extra > 0) history.go(-extra); }, 0);
}

if (typeof addEventListener === 'function') {
  addEventListener('popstate', () => {
    const target = depth();
    while (stack.length > target) stack.pop()!.back();
  });
}
