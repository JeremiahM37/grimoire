const SHORTCUTS: [string, string][] = [
  ['Ctrl / ⌘ K or O', 'Command palette: jump to a note or run a command'],
  ['Ctrl / ⌘ P', 'Commands only'],
  ['Ctrl / ⌘ S', 'Save the current note'],
  ['Ctrl / ⌘ F', 'Find in the current note'],
  ['Ctrl / ⌘ E', 'Switch between reading and editing'],
  ['Ctrl / ⌘ G', 'Graph view'],
  ['Ctrl / ⌘ \\', 'Show or hide the sidebar'],
  ['Alt + N', 'New note'],
  ['?', 'This help (outside a text field)'],
  ['Escape', 'Close a panel, or clear a search'],
  ['↑ / ↓ / Enter', 'Move through a list and open the selection'],
];

export function HelpPanel({ close }: { close: () => void }) {
  return <div id="help-modal" className="modal" role="dialog" aria-label="Keyboard shortcuts" onMouseDown={event => event.currentTarget === event.target && close()}><div className="modal-box"><button id="help-close" className="icon modal-close" aria-label="Close keyboard shortcuts" onClick={close}>✕</button><div id="help-body"><h2>Keyboard shortcuts</h2><dl className="help-keys">{SHORTCUTS.flatMap(([keys, action]) => [<dt key={keys + ':k'}>{keys}</dt>, <dd key={keys + ':d'}>{action}</dd>])}</dl></div></div></div>;
}
