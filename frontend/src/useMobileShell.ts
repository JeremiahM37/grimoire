import {useEffect,type Dispatch,type SetStateAction} from 'react';
export function useMobileShell(setOpen:Dispatch<SetStateAction<boolean>>){
 useEffect(()=>{
  let start:{x:number;y:number;side:boolean;editable:boolean}|undefined;
  const begin=(event:TouchEvent)=>{if(innerWidth>780||event.touches.length!==1)return;const point=event.touches[0]!,target=event.target as Element;start={x:point.clientX,y:point.clientY,side:!!target.closest('#sidebar'),editable:!!target.closest('input,textarea,[contenteditable=true]')};};
  const end=(event:TouchEvent)=>{const gesture=start;start=undefined;if(!gesture||gesture.editable||!event.changedTouches.length)return;const point=event.changedTouches[0]!,dx=point.clientX-gesture.x,dy=point.clientY-gesture.y;if(Math.abs(dx)<70||Math.abs(dx)<Math.abs(dy)*1.5)return;if(gesture.x<=24&&dx>0)setOpen(true);else if(gesture.side&&dx<0)setOpen(false);};
  const viewport=()=>document.documentElement.style.setProperty('--visible-height',`${Math.round(window.visualViewport?.height||innerHeight)}px`);
  viewport();addEventListener('resize',viewport);window.visualViewport?.addEventListener('resize',viewport);document.addEventListener('touchstart',begin,{passive:true});document.addEventListener('touchend',end,{passive:true});
  return()=>{removeEventListener('resize',viewport);window.visualViewport?.removeEventListener('resize',viewport);document.removeEventListener('touchstart',begin);document.removeEventListener('touchend',end);};
 },[setOpen]);
}
