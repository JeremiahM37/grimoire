import {useEffect,type Dispatch,type SetStateAction} from 'react';
// Edge swipe back (note -> list) for browsers without a system back gesture.
// Also tracks the visual viewport so sheets and the pinned editor toolbar sit
// above the on-screen keyboard.
export function useMobileShell(setOpen:Dispatch<SetStateAction<boolean>>){
 useEffect(()=>{
  let start:{x:number;y:number;editable:boolean}|undefined;
  const begin=(event:TouchEvent)=>{if(innerWidth>780||event.touches.length!==1)return;const point=event.touches[0]!,target=event.target as Element;start={x:point.clientX,y:point.clientY,editable:!!target.closest('input,textarea,[contenteditable=true],.cm-editor,#graph-canvas')&&point.clientX>32};};
  const end=(event:TouchEvent)=>{const gesture=start;start=undefined;if(!gesture||gesture.editable||!event.changedTouches.length)return;const point=event.changedTouches[0]!,dx=point.clientX-gesture.x,dy=point.clientY-gesture.y;if(Math.abs(dx)<70||Math.abs(dx)<Math.abs(dy)*1.5)return;if(gesture.x<=32&&dx>0&&!document.querySelector('.modal'))setOpen(true);};
  const viewport=()=>{const vv=window.visualViewport,root=document.documentElement.style;root.setProperty('--visible-height',`${Math.round(vv?.height||innerHeight)}px`);root.setProperty('--kb',`${Math.max(0,Math.round(innerHeight-(vv?.height||innerHeight)-(vv?.offsetTop||0)))}px`);};
  viewport();addEventListener('resize',viewport);window.visualViewport?.addEventListener('resize',viewport);window.visualViewport?.addEventListener('scroll',viewport);document.addEventListener('touchstart',begin,{passive:true});document.addEventListener('touchend',end,{passive:true});
  const focusIn=(event:FocusEvent)=>{if(innerWidth<=780&&(event.target as Element|null)?.closest?.('#live-editor,#content'))document.body.classList.add('editing');};
  const focusOut=()=>setTimeout(()=>{const a=document.activeElement;if(!a||!a.closest('#live-editor,#content,#ed-toolbar'))document.body.classList.remove('editing');},120);
  document.addEventListener('focusin',focusIn);document.addEventListener('focusout',focusOut);
  return()=>{removeEventListener('resize',viewport);window.visualViewport?.removeEventListener('resize',viewport);window.visualViewport?.removeEventListener('scroll',viewport);document.removeEventListener('touchstart',begin);document.removeEventListener('touchend',end);document.removeEventListener('focusin',focusIn);document.removeEventListener('focusout',focusOut);};
 },[setOpen]);
}
