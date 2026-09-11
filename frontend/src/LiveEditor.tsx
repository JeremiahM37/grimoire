import { useEffect, useRef, useState, type MutableRefObject } from "react";
export interface LiveAdapter {
  view: {
    dispatch: (change: { selection: { anchor: number; head: number } }) => void;
  };
  getValue: () => string;
  setValue: (value: string) => void;
  destroy: () => void;
  focus: () => void;
  getSelection: () => { from: number; to: number; text: string };
  replaceSelection: (value: string, select?: boolean) => void;
  setCursor: (position: number) => void;
}
interface Snippet {
  name: string;
  detail?: string;
  insert?: string;
  run?: () => void;
}
interface Callbacks {
  onChange: (body: string) => void;
  onSave: () => void;
  onOpenLink: (path: string, options?: { split?: boolean }) => void;
  onTagClick: (tag: string) => void;
  getLinkCompletions: (query: string) => string[];
  getTagCompletions: (query: string) => string[];
  getSlashCommands: (query: string) => Snippet[];
  onFiles: (files: File[]) => void;
}
interface Props {
  value: string;
  readOnly: boolean;
  callbacks: Callbacks;
  adapterRef: MutableRefObject<LiveAdapter | undefined>;
}
export function LiveEditor({ value, readOnly, callbacks, adapterRef }: Props) {
  const host = useRef<HTMLDivElement>(null),
    latest = useRef({ value, readOnly, callbacks }),
    syncing = useRef(false),
    [error, setError] = useState("");
  latest.current = { value, readOnly, callbacks };
  useEffect(() => {
    if (readOnly || localStorage.getItem("grimoire-editor-mode") === "classic")
      return;
    let cancelled = false;
    const url = "/vendor/editor.js";
    void import(/* @vite-ignore */ url)
      .then(
        (module: {
          createLiveEditor: (options: {
            parent: HTMLElement;
            doc: string;
            callbacks: Callbacks;
          }) => LiveAdapter;
        }) => {
          if (cancelled || !host.current) return;
          const invoke: Callbacks = {
            onChange: (body) => {
              if (!syncing.current && !latest.current.readOnly)
                latest.current.callbacks.onChange(body);
            },
            onSave: () => latest.current.callbacks.onSave(),
            onOpenLink: (path, options) =>
              latest.current.callbacks.onOpenLink(path, options),
            onTagClick: (tag) => latest.current.callbacks.onTagClick(tag),
            getLinkCompletions: (q) =>
              latest.current.callbacks.getLinkCompletions(q),
            getTagCompletions: (q) =>
              latest.current.callbacks.getTagCompletions(q),
            getSlashCommands: (q) =>
              latest.current.callbacks.getSlashCommands(q),
            onFiles: (files) => latest.current.callbacks.onFiles(files),
          };
          adapterRef.current = module.createLiveEditor({
            parent: host.current,
            doc: latest.current.value,
            callbacks: invoke,
          });
          document.body.classList.add("live-editor-on");
        },
      )
      .catch((error) => {
        if (!cancelled)
          setError(
            "Live editor unavailable; plain-text editing remains available. " +
              String(error),
          );
      });
    return () => {
      cancelled = true;
      adapterRef.current?.destroy();
      adapterRef.current = undefined;
      document.body.classList.remove("live-editor-on");
    };
  }, [readOnly, adapterRef]);
  useEffect(() => {
    const adapter = adapterRef.current;
    if (adapter && adapter.getValue() !== value) {
      syncing.current = true;
      try {
        const selection = adapter.getSelection();
        adapter.setValue(value);
        adapter.view.dispatch({
          selection: {
            anchor: Math.min(selection.from, value.length),
            head: Math.min(selection.to, value.length),
          },
        });
      } finally {
        syncing.current = false;
      }
    }
  }, [value, adapterRef]);
  return (
    <>
      {error && <p role="status">{error}</p>}
      <div id="live-editor" ref={host} />
    </>
  );
}
