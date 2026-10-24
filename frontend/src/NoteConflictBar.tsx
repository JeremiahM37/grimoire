// The bar shown when a save was refused because the note changed elsewhere.
// Nothing is overwritten until the user picks one of the choices.
export function NoteConflictBar({ hasCurrent, choose }: { hasCurrent: boolean; choose: (choice: 'theirs' | 'mine' | 'both') => void }) {
  return (
    <div className="note-conflict-bar" role="alert">
      <span>{hasCurrent ? 'This note changed elsewhere while you were editing. Your edits are held here.' : 'This note was removed or moved while you were editing.'}</span>
      {hasCurrent && <button type="button" onClick={() => choose('theirs')}>Reload theirs</button>}
      <button type="button" onClick={() => choose('mine')}>Keep mine</button>
      {hasCurrent && <button type="button" onClick={() => choose('both')}>Keep both</button>}
    </div>
  );
}
