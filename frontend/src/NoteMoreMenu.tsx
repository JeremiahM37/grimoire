import { useEffect, useRef, useState, type ReactNode } from 'react';

// The note header's overflow ("⋯") menu. Everything that is not a primary action lives here,
// with a text label, instead of as an unlabelled icon in the header.
export function NoteMoreMenu({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!open) return;
    const outside = (event: MouseEvent) => { if (!wrap.current?.contains(event.target as Node)) setOpen(false); };
    const escape = (event: KeyboardEvent) => { if (event.key === 'Escape') { setOpen(false); button.current?.focus(); } };
    document.addEventListener('mousedown', outside);
    addEventListener('keydown', escape);
    return () => { document.removeEventListener('mousedown', outside); removeEventListener('keydown', escape); };
  }, [open]);
  return <div className="ed-more" ref={wrap}>
    <button ref={button} id="note-more" className="icon" aria-label="More options" title="More options" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(value => !value)}>⋯</button>
    {open && <div className="menu ed-menu" role="menu" onClick={() => setOpen(false)}>{children}</div>}
  </div>;
}
