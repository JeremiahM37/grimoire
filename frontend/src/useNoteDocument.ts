import { useCallback, useEffect, useRef, useState } from "react";
import type { Note } from "./types";
interface API {
  note: (path: string) => Promise<Note>;
  update: (note: Note) => Promise<Note>;
}
const draftKey = (path: string) => "grimoire-react-draft:" + path;
function persist(note: Note) {
  try {
    if (note.encrypted || note.locked) {
      localStorage.removeItem(draftKey(note.path));
      return;
    }
    localStorage.setItem(
      draftKey(note.path),
      JSON.stringify({ title: note.title, body: note.body }),
    );
  } catch {}
}
export function useNoteDocument(
  api: API,
  onSaved: () => Promise<void>,
  onError: (message: string) => void,
  syncHash = true,
) {
  const [active, render] = useState<Note>(),
    [dirty, renderDirty] = useState(false),
    [saveState, setSaveState] = useState("");
  const activeRef = useRef<Note | undefined>(undefined),
    dirtyRef = useRef(false),
    revision = useRef(0),
    navigation = useRef(0),
    timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined),
    pending = useRef<Promise<boolean> | undefined>(undefined),
    callbacks = useRef({ onSaved, onError });
  callbacks.current = { onSaved, onError };
  const setDirty = useCallback((value: boolean) => {
    dirtyRef.current = value;
    renderDirty(value);
  }, []);
  const setActive = useCallback(
    (note: Note | undefined) => {
      clearTimeout(timer.current);
      revision.current++;
      activeRef.current = note;
      render(note);
      setDirty(false);
      setSaveState("");
    },
    [setDirty],
  );
  const save = useCallback((): Promise<boolean> => {
    clearTimeout(timer.current);
    if (pending.current) return pending.current;
    const run = async () => {
      while (
        dirtyRef.current &&
        activeRef.current &&
        !activeRef.current.locked
      ) {
        const note = activeRef.current,
          version = revision.current;
        setSaveState("saving…");
        try {
          const saved = await api.update(note);
          if (activeRef.current?.path !== note.path) return true;
          if (revision.current === version) {
            // A storage newline must not move the user's cursor or selection.
            const current = saved.body.trimEnd() === note.body.trimEnd() ? {...saved, body: note.body} : saved;
            activeRef.current = current;
            render(current);
            setDirty(false);
            setSaveState("saved");
            try {
              localStorage.removeItem(draftKey(note.path));
              const old = JSON.parse(
                localStorage.getItem("grimoire-draft") || "null",
              );
              if (old?.path === note.path)
                localStorage.removeItem("grimoire-draft");
            } catch {}
          } else {
            const current = {
              ...saved,
              ...activeRef.current,
              hash: saved.hash,
              mtime: saved.mtime,
              updated: saved.updated,
            };
            activeRef.current = current;
            render(current);
            persist(current);
          }
          void callbacks.current.onSaved().catch(() => undefined);
        } catch (error) {
          setSaveState(note.encrypted ? "failed · unsaved" : "failed · draft kept");
          if (activeRef.current?.path === note.path) persist(activeRef.current);
          callbacks.current.onError(
            error instanceof Error ? error.message : "Save failed",
          );
          return false;
        }
      }
      return true;
    };
    pending.current = run().finally(() => {
      pending.current = undefined;
    });
    return pending.current;
  }, [api, setDirty]);
  const edit = useCallback(
    (change: Partial<Note>) => {
      const note = activeRef.current;
      if (!note || note.locked) return;
      const next = { ...note, ...change };
      revision.current++;
      activeRef.current = next;
      render(next);
      setDirty(true);
      setSaveState("editing");
      persist(next);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => void save(), 350);
    },
    [save, setDirty],
  );
  const open = useCallback(
    async (path: string) => {
      const request = ++navigation.current;
      if (!(await save()) || request !== navigation.current) return;
      try {
        const note = await api.note(path);
        if (request !== navigation.current) return;
        // An edit made while the requested note was loading must be saved first.
        if (!(await save()) || request !== navigation.current) return;
        setActive(note);
        if (!note.encrypted && !note.locked) {
          try {
            let raw = localStorage.getItem(draftKey(note.path));
            if (!raw) {
              const legacy = JSON.parse(
                localStorage.getItem("grimoire-draft") || "null",
              );
              if (legacy?.path === note.path)
                raw = JSON.stringify({
                  body: legacy.content,
                  title: legacy.title,
                });
            }
            if (raw) {
              const draft: unknown = JSON.parse(raw);
              if (
                draft &&
                typeof draft === "object" &&
                "body" in draft &&
                typeof draft.body === "string" &&
                "title" in draft &&
                typeof draft.title === "string"
              ) {
                edit({ body: draft.body, title: draft.title });
                setSaveState("draft restored");
              }
            }
          } catch {}
        }
        if(syncHash) history.replaceState(null, "", "#" + encodeURIComponent(note.path));
      } catch (error) {
        callbacks.current.onError(
          error instanceof Error ? error.message : "Could not open note",
        );
      }
    },
    [api, save, setActive, edit, syncHash],
  );
  useEffect(() => {
    const online = () => {
      if (dirtyRef.current) void save();
    };
    const leave = (event: BeforeUnloadEvent) => {
      if (dirtyRef.current) {
        event.preventDefault();
        event.returnValue = "";
      }
    };
    addEventListener("online", online);
    addEventListener("beforeunload", leave);
    return () => {
      clearTimeout(timer.current);
      removeEventListener("online", online);
      removeEventListener("beforeunload", leave);
    };
  }, [save]);
  return {
    active,
    setActive,
    activeRef,
    dirty,
    dirtyRef,
    setDirty,
    saveState,
    setSaveState,
    save,
    open,
    edit,
  };
}
