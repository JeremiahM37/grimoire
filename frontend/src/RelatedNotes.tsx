import {useEffect,useState} from 'react';
import type {SyntheticEvent} from 'react';
import {createGrimoireApi,noteURLPath} from './api';
import type {Note} from './types';
const api=createGrimoireApi();
const OPEN_KEY='grimoire-related-open';
interface Mention {path:string;title:string;name:string;context:string}
interface Connections {backlinks:{path:string;title:string}[];links:{target:string;dst:string;alias:string;resolved:number}[]}
const readOpen=():boolean=>{try{return localStorage.getItem(OPEN_KEY)==='1';}catch{return false;}};
const count=(n:number,one:string,many:string)=>`${n} ${n===1?one:many}`;
export function RelatedNotes({note,open}:{note?:Note;open:(path:string)=>void}){
 const [data,setData]=useState<Connections>({backlinks:[],links:[]}),[mentions,setMentions]=useState<Mention[]>([]),[version,setVersion]=useState(0),[busy,setBusy]=useState(''),[error,setError]=useState(''),[expanded,setExpanded]=useState(readOpen);
 useEffect(()=>{const abort=new AbortController();setError('');setData({backlinks:[],links:[]});setMentions([]);if(!note||note.locked)return;void Promise.all([api.request<Connections>('/notes/'+noteURLPath(note.path),{signal:abort.signal}),api.request<Mention[]>('/notes/'+noteURLPath(note.path)+'/unlinked',{signal:abort.signal})]).then(([links,rows])=>{setData(links);setMentions(rows);}).catch(e=>{if(!abort.signal.aborted)setError(String(e));});return()=>abort.abort();},[note?.path,note?.hash,note?.locked,version]);
 // The bar is the whole control: its summary carries the counts, and the choice to expand is remembered per device.
 const onToggle=(event:SyntheticEvent<HTMLDetailsElement>)=>{const next=event.currentTarget.open;if(next===expanded)return;setExpanded(next);try{localStorage.setItem(OPEN_KEY,next?'1':'0');}catch{/* storage unavailable: the choice just is not remembered */}};
 const link=(path:string,title:string)=><a className="wikilink" href={'#'+encodeURIComponent(path)} onClick={e=>{e.preventDefault();open(path);}}>{title}</a>;
 const parts=[data.backlinks?.length?count(data.backlinks.length,'backlink','backlinks'):'',data.links?.length?count(data.links.length,'link','links'):'',mentions.length?count(mentions.length,'mention','mentions'):''].filter(Boolean);
 if(!parts.length&&!error)return null;
 return <details id="note-connections" open={expanded} onToggle={onToggle}><summary><span className="rel-caret" aria-hidden="true">▸</span>{parts.length?parts.join(' · '):'Related notes'}</summary><div className="connections-body"><section id="backlinks">{!!data.backlinks?.length&&<h4>Linked from</h4>}{data.backlinks?.map(row=><div key={row.path}>{link(row.path,row.title)}</div>)}{!!data.links?.length&&<div className="outgoing"><h4>Links in this note</h4>{data.links.map((row,i)=><div key={i}>{row.resolved?link(row.dst,row.alias||row.target):<span className="unresolved">{row.alias||row.target}</span>}</div>)}</div>}</section><section id="unlinked">{!!mentions.length&&<h4>Unlinked mentions</h4>}{mentions.map(row=><div className="unlinked-row" key={row.path}><div className="ul-top">{link(row.path,row.title)}<button className="link-btn" disabled={!!busy} onClick={()=>{if(!note)return;setBusy(row.path);void api.request('/notes/'+noteURLPath(note.path)+'/link',{method:'POST',body:{source:row.path,name:row.name}}).then(()=>setVersion(v=>v+1)).catch(e=>setError(String(e))).finally(()=>setBusy(''));}}>{busy===row.path?'Linking…':'🔗 link'}</button></div><div className="ctx">{row.context}</div></div>)}</section>{error&&<p role="status">{error}</p>}</div></details>;
}
