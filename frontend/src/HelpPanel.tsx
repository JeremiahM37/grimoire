const SHORTCUTS: [string, string][] = [
  ['Ctrl / ⌘ K or O', 'Command palette: recent notes, every note, and commands'],
  ['Ctrl / ⌘ P', 'Command palette showing commands only'],
  ['Ctrl / ⌘ S', 'Save the current note'],
  ['Ctrl / ⌘ F', 'Find in the current note'],
  ['Ctrl / ⌘ E', 'Switch between reading and editing the current note'],
  ['Ctrl / ⌘ G', 'Open the graph view'],
  ['Ctrl / ⌘ \\', 'Show or hide the sidebar'],
  ['Alt + N', 'New note'],
  ['?', 'Open this keyboard help outside an editor field'],
  ['Escape', 'Close a panel or the palette, or clear graph search'],
  ['↑ / ↓ / Enter', 'In the palette: move the selection and run it'],
];

export function HelpPanel({ close }: { close: () => void }) {
  return <div id="help-modal" className="modal" role="dialog" aria-label="Keyboard shortcuts" onMouseDown={event => event.currentTarget === event.target && close()}><div className="modal-box"><button id="help-close" className="icon modal-close" aria-label="Close keyboard shortcuts" onClick={close}>✕</button><div id="help-body"><h2>Keyboard shortcuts</h2><dl className="help-keys">{SHORTCUTS.flatMap(([keys, action]) => [<dt key={keys + ':k'}>{keys}</dt>, <dd key={keys + ':d'}>{action}</dd>])}</dl></div></div></div>;
}
