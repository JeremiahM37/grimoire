import { useEffect, useRef, useState } from 'react';
import type { RequestOptions } from './api';
import type { Note, Plugin } from './types';

export type PluginCommand = { icon?: string; name: string; run: () => void | Promise<void> };
export type PluginSnippet = { name: string; detail?: string; insert: string | (() => string) };
export const pluginContrib = { commands: [] as PluginCommand[], snippets: [] as PluginSnippet[], fences: new Map<string, (el: HTMLElement, source: string) => void | Promise<void>>(), transforms: [] as ((root: HTMLElement) => void | Promise<void>)[] };
export const pluginsChanged = () => dispatchEvent(new Event('grimoire-plugins'));
type Panel = { id: string; title: string; render: (el: HTMLElement) => void | Promise<void> };
type EventName = 'boot' | 'note-open' | 'note-save';
type Request = <T>(path: string, init?: RequestOptions) => Promise<T>;
type Props = { request: Request; open: (path: string) => void | Promise<void>; currentNote: () => Pick<Note, 'path' | 'title' | 'body'> | null; insertText: (text: string) => void; toast: (message: string, error?: boolean) => void };
const asset = (name: string, rel: string) => `/plugins/${encodeURIComponent(name)}/${rel}`;
const script = (url: string) => new Promise<void>((resolve, reject) => { const node = document.createElement('script'); node.src = url; node.onload = () => resolve(); node.onerror = () => reject(new Error(`plugin asset failed: ${url}`)); document.head.appendChild(node); });
const styles = (url: string) => { const node = document.createElement('link'); node.rel = 'stylesheet'; node.href = url; document.head.appendChild(node); };

/** State-backed host bridge for the public browser plugin API. */
export function PluginRuntime({ request, open, currentNote, insertText, toast }: Props) {
  const [panels, setPanels] = useState<Panel[]>([]);
  const host = useRef({ request, open, currentNote, insertText, toast }); host.current = { request, open, currentNote, insertText, toast };
  const listeners = useRef(new Map<EventName, ((payload?: unknown) => void)[]>());
  useEffect(() => { const receive = (event: Event) => { const detail = (event as CustomEvent<{event: EventName; payload?: unknown}>).detail; for (const callback of listeners.current.get(detail?.event) || []) try { callback(detail.payload); } catch (error) { console.error(`plugin ${detail.event} handler:`, error); } }; addEventListener('grimoire-plugin-event', receive); return () => removeEventListener('grimoire-plugin-event', receive); }, []);
  useEffect(() => { let live = true; const base = {
    api: <T,>(path: string, init?: RequestOptions) => host.current.request<T>(path, init), toast: (message: string, error?: boolean) => host.current.toast(message, error), openNote: (path: string) => host.current.open(path), getCurrentNote: () => host.current.currentNote(), insertText: (text: string) => host.current.insertText(text),
    registerPanel: (panel: Panel) => { if (live) setPanels(rows => rows.some(row => row.id === panel.id) ? rows : [...rows, panel]); },
    registerCommand: (command: PluginCommand) => { pluginContrib.commands.push({ icon: command.icon || '🔌', ...command }); pluginsChanged(); }, registerSlashSnippet: (snippet: PluginSnippet) => { pluginContrib.snippets.push(snippet); pluginsChanged(); }, registerFenceRenderer: (lang: string, render: (el: HTMLElement, source: string) => void | Promise<void>) => { pluginContrib.fences.set(lang, render); pluginsChanged(); }, registerPreviewTransform: (transform: (root: HTMLElement) => void | Promise<void>) => { pluginContrib.transforms.push(transform); pluginsChanged(); },
    on: (event: EventName, callback: (payload?: unknown) => void) => listeners.current.set(event, [...(listeners.current.get(event) || []), callback]),
  }; void (async () => { try { const rows = await host.current.request<Plugin[]>('/plugins'); for (const plugin of rows.filter(row => row.enabled && row.client_url)) try { if (plugin.styles_url) styles(plugin.styles_url); const module = await import(/* @vite-ignore */ plugin.client_url!); const activate = (module as {activate?:(host:unknown)=>unknown;default?:(host:unknown)=>unknown}).activate || (module as {default?:(host:unknown)=>unknown}).default; await activate?.({...base, name: plugin.name, loadScript: (rel:string) => script(asset(plugin.name, rel)), loadStyles: (rel:string) => styles(asset(plugin.name, rel)), assetUrl: (rel:string) => asset(plugin.name, rel)}); } catch (error) { console.error(`plugin \"${plugin.name}\" failed to activate:`, error); } if (live) dispatchEvent(new CustomEvent('grimoire-plugin-event', {detail:{event:'boot'}})); } catch {} })(); return () => { live = false; }; }, []);
  return <div id="plugin-panels">{panels.map(panel => <PluginMount key={panel.id} panel={panel}/>)}</div>;
}
function PluginMount({panel}:{panel:Panel}) { const ref=useRef<HTMLDivElement>(null); useEffect(()=>{ if(ref.current) void Promise.resolve(panel.render(ref.current)).catch(error=>{if(ref.current)ref.current.textContent=`panel failed: ${error instanceof Error ? error.message : String(error)}`;}); },[panel]); return <section className="plugin-panel"><div className="pp-title">{panel.title}</div><div className="pp-body" ref={ref}/></section>; }
