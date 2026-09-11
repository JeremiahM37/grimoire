import {useEffect,useState,type ReactNode} from 'react';
export function Slides({body,render,close}:{body:string;render:(body:string)=>ReactNode;close:()=>void}){
 const pages=body.split(/^\s*---+\s*$/m);const[index,setIndex]=useState(0);
 useEffect(()=>{const key=(e:KeyboardEvent)=>{if(['ArrowRight',' ','PageDown'].includes(e.key)){e.preventDefault();setIndex(i=>Math.min(pages.length-1,i+1));}if(['ArrowLeft','PageUp'].includes(e.key)){e.preventDefault();setIndex(i=>Math.max(0,i-1));}if(e.key==='Escape')close();};addEventListener('keydown',key);return()=>removeEventListener('keydown',key);},[pages.length,close]);
 return <div id="slides" role="dialog" aria-modal="true" aria-label="Presentation"><div className="slide">{render(pages[index]||'')}</div><footer><button aria-label="Previous slide" disabled={index===0} onClick={()=>setIndex(i=>i-1)}>←</button><span>{index+1} / {pages.length}</span><button aria-label="Next slide" disabled={index===pages.length-1} onClick={()=>setIndex(i=>i+1)}>→</button><button onClick={close}>Close</button></footer></div>;
}
