import {useEffect,useState} from 'react';
import {createGrimoireApi} from './api';
import type {Note} from './types';
import {useNoteDocument} from './useNoteDocument';
export function SplitEditor({note,api,refresh,close,resize}:{note:Note;api:ReturnType<typeof createGrimoireApi>;refresh:()=>Promise<void>;close:()=>void;resize:React.ReactNode}){
 const [error,setError]=useState('');const document=useNoteDocument(api,refresh,setError,false);
 useEffect(()=>{void document.open(note.path);},[note.path,document.open]);
 return <aside id="editor2"><header><input id="title2" aria-label="Second note title" value={document.active?.title||''} readOnly={document.active?.locked} onChange={e=>document.edit({title:e.target.value})}/><span id="save-state2">{document.saveState}</span><button id="editor2-close" aria-label="Close second editor" onClick={()=>void document.save().then(ok=>{if(ok)close();})}>×</button></header><textarea id="content2" aria-label="Second note body" value={document.active?.body||''} readOnly={document.active?.locked} onChange={e=>document.edit({body:e.target.value})}/>{error&&<p role="alert">{error}</p>}{resize}</aside>;
}
export function ResizeHandle({id,value,onChange}:{id:string;value:()=>number;onChange:(value:number)=>void}){
 return <div id={id} className="resize-h" role="separator" aria-orientation="vertical" aria-label={id==='sidebar-resize'?'Sidebar width':'Editor width'} tabIndex={0} onKeyDown={e=>{if(e.key==='ArrowLeft'||e.key==='ArrowRight'){e.preventDefault();onChange(value()+(e.key==='ArrowRight'?20:-20));}}} onPointerDown={e=>{e.preventDefault();const element=e.currentTarget,start=e.clientX,width=value();element.setPointerCapture(e.pointerId);const move=(event:PointerEvent)=>onChange(width+event.clientX-start);const stop=()=>{element.removeEventListener('pointermove',move);element.removeEventListener('pointerup',stop);element.removeEventListener('pointercancel',stop);};element.addEventListener('pointermove',move);element.addEventListener('pointerup',stop);element.addEventListener('pointercancel',stop);}}/>;
}
