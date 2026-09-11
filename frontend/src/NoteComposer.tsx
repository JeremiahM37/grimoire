import {useEffect,useRef,useState} from 'react';
import {createGrimoireApi,noteURLPath} from './api';
import type {Note,NoteListItem} from './types';
export type Composition={kind:'extract'|'merge';note:Note;from:number;to:number};
export function NoteComposer({operation,api,notes,save,edit,refresh,open,close}:{operation:Composition;api:ReturnType<typeof createGrimoireApi>;notes:NoteListItem[];save:()=>Promise<boolean>;edit:(change:Partial<Note>)=>void;refresh:()=>Promise<void>;open:(path:string)=>Promise<unknown>;close:()=>void}){
 const dialog=useRef<HTMLDialogElement>(null);const[value,setValue]=useState(''),[target,setTarget]=useState<NoteListItem>(),[busy,setBusy]=useState(false),[error,setError]=useState('');const appended=useRef(false),created=useRef<Note|undefined>(undefined);
 useEffect(()=>{dialog.current?.showModal();},[]);
 async function submit(){if(busy)return;setError('');if(!value.trim()){setError('Enter a note title.');return;}if(operation.kind==='merge'&&!target){const hit=notes.find(n=>[n.title,n.path].some(name=>name?.toLowerCase()===value.trim().toLowerCase()));if(!hit){setError('No note with that title.');return;}if(hit.path===operation.note.path){setError('A note cannot be merged into itself.');return;}setTarget(hit);return;}
 setBusy(true);try{if(!await save())throw new Error('Save the current note before continuing.');
 if(operation.kind==='extract'){
  const latest=await api.note(operation.note.path);if(latest.body.trimEnd()!==operation.note.body.trimEnd())throw new Error('The note changed. Close this dialog and select the text again.');
  const selected=operation.note.body.slice(operation.from,operation.to);if(!created.current)created.current=await api.create({title:value.trim(),body:selected.trim()+'\n'});
  edit({body:operation.note.body.slice(0,operation.from)+`[[${created.current.title}]]`+operation.note.body.slice(operation.to)});await refresh();close();
 }else if(target){
  if(!appended.current){const source=await api.note(operation.note.path),destination=await api.note(target.path);if(source.locked||destination.locked)throw new Error('Unlock both notes before merging.');await api.request(`/notes/${noteURLPath(target.path)}`,{method:'PUT',body:{body:destination.body.replace(/\n+$/,'')+`\n\n## ${source.title}\n\n`+source.body,frontmatter:destination.frontmatter}});appended.current=true;}
  await api.request(`/notes/${noteURLPath(operation.note.path)}`,{method:'DELETE'});await refresh();await open(target.path);close();
 }
 }catch(e){setError((appended.current?'Content was appended. Retry to move the source to trash. ':'')+String(e));}finally{setBusy(false);}}
 return <dialog ref={dialog} className="react-note-dialog" onCancel={e=>{if(busy)e.preventDefault();else close();}}><form className="form-panel" key={target?'confirm':'title'} onSubmit={e=>{e.preventDefault();void submit();}}><h2>{operation.kind==='extract'?'Extract selection':target?'Confirm merge':'Merge this note'}</h2>{target?<p>Append this note to “{target.title}” and move the source to trash?</p>:<label>{operation.kind==='extract'?'Title for the extracted note':'Merge into note (title)'}<input autoFocus value={value} disabled={busy} onChange={e=>setValue(e.target.value)}/></label>}{error&&<p role="alert">{error}</p>}<button type="submit" disabled={busy}>{busy?'Working…':target?'Merge notes':'Continue'}</button><button type="button" disabled={busy} onClick={close}>Cancel</button></form></dialog>;
}
