import { useCallback, useEffect, useRef, useState } from "react";
import type { Note } from "./types";
import { conflictFrom, keepBoth, keepMine, type NoteConflict } from "./noteConflict";
interface API {
  note: (path: string) => Promise<Note>;
  update: (note: Note) => Promise<Note>;
}
const draftKey = (path: string) => "grimoire-react-draft:" + path;
function persist(note: Note) {
  try {
    if (note.encrypted || note.locked) {
      localStorage.removeItem(draftKey(note.path));
      const legacy=JSON.parse(localStorage.getItem('grimoire-draft')||'null');
      if(legacy?.path===note.path)localStorage.removeItem('grimoire-draft');
      return;
    }
    // Keep the prior console's single-draft format readable during upgrades.
    localStorage.setItem('grimoire-draft',JSON.stringify({path:note.path,title:note.title,content:note.body}));
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
    [saveState, setSaveState] = useState(""),
    [conflict, setConflict] = useState<NoteConflict>();
  const activeRef = useRef<Note | undefined>(undefined),
    dirtyRef = useRef(false),
    // While a conflict is open nothing saves, so autosave cannot retry over it.
    conflictRef = useRef<NoteConflict | undefined>(undefined),
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
      // Refusing here (not returning true) also keeps navigation from moving
      // the user off a note whose edits have not been saved yet.
      if (conflictRef.current) return false;
      while (
        dirtyRef.current &&
        activeRef.current &&
        !activeRef.current.locked &&
        !conflictRef.current
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
          const hit =
            activeRef.current?.path === note.path
              ? conflictFrom(activeRef.current, error)
              : undefined;
          if (hit) {
            // The note changed elsewhere. Nothing was written: hold the local
            // edits and let the user choose, instead of retrying.
            conflictRef.current = hit;
            setConflict(hit);
            setSaveState("conflict");
            return false;
          }
          setSaveState((navigator.onLine ? "failed" : "offline") + (note.encrypted ? " · unsaved" : " · draft kept"));
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
  // Resolves an open conflict. "theirs" discards local edits; "mine" and "both"
  // re-save on top of the current version.
  const resolveConflict = useCallback(
    async (choice: "theirs" | "mine" | "both") => {
      const hit = conflictRef.current,
        note = activeRef.current;
      if (!hit || !note) return;
      if (choice === "theirs") {
        if (!hit.current) return;
        try {
          localStorage.removeItem(draftKey(note.path));
        } catch {}
        conflictRef.current = undefined;
        setConflict(undefined);
        setActive(hit.current);
        return;
      }
      const next =
        choice === "mine"
          ? keepMine(note, hit.current)
          : hit.current && keepBoth(note, hit.current);
      if (!next) return;
      activeRef.current = next;
      render(next);
      conflictRef.current = undefined;
      setConflict(undefined);
      setDirty(true);
      await save();
    },
    [save, setActive, setDirty],
  );
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
    conflict,
    resolveConflict,
  };
}
