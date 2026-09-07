/** A navigable note map. Layout is bounded; camera movement never reruns physics. */
import { $, api } from '/util.js';
let openNoteFn=()=>{}, currentPathFn=()=>null, dispose=()=>{}, generation=0;
export function initGraph({openNote,currentPath}) { openNoteFn=openNote;currentPathFn=currentPath; }
function closeGraph() { generation++;dispose();dispose=()=>{};$('#graph-modal').classList.add('hidden');$('#graph-open').focus(); }
$('#graph-open').onclick=openGraph;
$('#graph-close').onclick=closeGraph;
$('#graph-modal').onclick=e=>{if(e.target.id==='graph-modal')closeGraph();};
addEventListener('keydown',e=>{if(e.key==='Escape'&&!$('#graph-modal').classList.contains('hidden')){e.preventDefault();e.stopPropagation();closeGraph();}},true);
export async function openGraph() {
  dispose();const ticket=++generation;
  const modal=$('#graph-modal');modal.classList.remove('hidden');
  $('#graph-stat').textContent='Loading…';$('#graph-selection').textContent='Select a note to see its connections.';$('#graph-results').replaceChildren();
  $('#graph-search').value='';$('#graph-close').focus();
  let g;
  try {g=await api('/graph');} catch(e) {if(ticket===generation)$('#graph-stat').textContent=`Could not load graph: ${e.message}. Close and reopen to retry.`;return;}
  if(ticket!==generation)return;
  const cv=$('#graph-canvas'),ctx=cv.getContext('2d');
  const all=new Map(g.nodes.map(n=>[n.id,{...n,neighbors:new Set()}]));
  const links=g.edges.filter(e=>all.has(e.src)&&all.has(e.dst)&&e.src!==e.dst);
  links.forEach(e=>{all.get(e.src).neighbors.add(e.dst);all.get(e.dst).neighbors.add(e.src);});
  let nodes=[],edges=[],selected=null,hover=null,w=1,h=1,camera={x:0,y:0,z:1},frame=0,ticks=0;
  const css=getComputedStyle(document.body), colors={ink:css.getPropertyValue('--ink').trim(),line:css.getPropertyValue('--line').trim(),accent:css.getPropertyValue('--accent').trim(),link:css.getPropertyValue('--link').trim(),paper:css.getPropertyValue('--paper').trim()};
  function fit(){if(!nodes.length)return draw();const xs=nodes.map(n=>n.x),ys=nodes.map(n=>n.y);const loX=Math.min(...xs),hiX=Math.max(...xs),loY=Math.min(...ys),hiY=Math.max(...ys);camera.z=Math.min(1.5,Math.max(.08,Math.min((w-70)/(hiX-loX+80),(h-70)/(hiY-loY+80))));camera.x=w/2-(loX+hiX)/2*camera.z;camera.y=h/2-(loY+hiY)/2*camera.z;draw();}
  function draw(){ctx.clearRect(0,0,w,h);const focus=selected||hover;ctx.lineWidth=1;
    for(const [a,b] of edges){ctx.globalAlpha=focus&&(a.id!==focus&&b.id!==focus)?.18:.65;ctx.strokeStyle=focus&&(a.id===focus||b.id===focus)?colors.accent:colors.line;ctx.beginPath();ctx.moveTo(a.x*camera.z+camera.x,a.y*camera.z+camera.y);ctx.lineTo(b.x*camera.z+camera.x,b.y*camera.z+camera.y);ctx.stroke();}
    const labels=[];
    for(const n of nodes){const x=n.x*camera.z+camera.x,y=n.y*camera.z+camera.y,r=5+Math.min(6,n.neighbors.size);if(x<-30||x>w+30||y<-30||y>h+30)continue;
      const related=!focus||n.id===focus||all.get(focus)?.neighbors.has(n.id);ctx.globalAlpha=related?1:.22;ctx.fillStyle=n.id===focus||n.id===currentPathFn()?colors.accent:colors.link;ctx.beginPath();ctx.arc(x,y,r,0,Math.PI*2);ctx.fill();
      if(n.id===focus||related&&(nodes.length<35||camera.z>1.3||n.neighbors.size>3))labels.push({n,x,y:y-r-8,priority:n.id===focus?1:0});
    }
    ctx.globalAlpha=1;ctx.font=`12px ${css.fontFamily}`;ctx.textAlign='center';const occupied=[];
    labels.sort((a,b)=>b.priority-a.priority).forEach(l=>{const text=l.n.title||l.n.id;const short=text.length>36?text.slice(0,35)+'…':text,tw=ctx.measureText(short).width;const box={x:l.x-tw/2-3,y:l.y-12,w:tw+6,h:17};if(!l.priority&&occupied.some(b=>box.x<b.x+b.w&&box.x+box.w>b.x&&box.y<b.y+b.h&&box.y+box.h>b.y))return;occupied.push(box);ctx.fillStyle=colors.paper;ctx.fillRect(box.x,box.y,box.w,box.h);ctx.fillStyle=colors.ink;ctx.fillText(short,l.x,l.y);});
    if(!nodes.length){ctx.fillStyle=colors.ink;ctx.font=`15px ${css.fontFamily}`;ctx.fillText('No notes in this view. Try All notes.',w/2,h/2);}
    cv.dataset.zoom=camera.z.toFixed(3);
  }
  function select(id){selected=id;const n=all.get(id);const panel=$('#graph-selection');panel.replaceChildren();if(!n)return;
    const title=document.createElement('strong');title.textContent=n.title||n.id;panel.append(title);
    const open=document.createElement('button');open.className='btn';open.textContent='Open note';open.onclick=()=>{closeGraph();openNoteFn(id);};panel.append(open);
    const count=document.createElement('span');count.textContent=`${n.neighbors.size} connected notes`;panel.append(count);
    [...n.neighbors].sort().forEach(key=>{const b=document.createElement('button');b.className='graph-neighbor';b.textContent=all.get(key).title||key;b.onclick=()=>focusNode(key);panel.append(b);});draw();
  }
  function focusNode(id){if(!nodes.some(n=>n.id===id)){$('#graph-scope').value='all';rebuild();}const n=nodes.find(n=>n.id===id);camera.z=Math.max(camera.z,1);camera.x=w/2-n.x*camera.z;camera.y=h/2-n.y*camera.z;select(id);}
  function rebuild(){cancelAnimationFrame(frame);ticks=0;const scope=$('#graph-scope').value,active=all.get(currentPathFn());
    nodes=[...all.values()].filter(n=>scope==='all'||scope==='local'?(scope==='all'||n.id===active?.id||active?.neighbors.has(n.id)):n.neighbors.size>0).map((n,i)=>({...n,x:Math.cos(i*2.399963)*50*Math.sqrt(i+1),y:Math.sin(i*2.399963)*50*Math.sqrt(i+1),vx:0,vy:0}));
    const index=new Map(nodes.map(n=>[n.id,n]));edges=links.filter(e=>index.has(e.src)&&index.has(e.dst)).map(e=>[index.get(e.src),index.get(e.dst)]);
    $('#graph-stat').textContent=`${nodes.length} notes · ${edges.length} links`;selected=null;hover=null;$('#graph-selection').textContent='Select a note to see its connections.';
    // Local spatial buckets bound repulsion work. No viewport clamping: fit the camera instead.
    function step(){const alpha=1-ticks/90,buckets=new Map();nodes.forEach(n=>{const k=`${Math.floor(n.x/120)},${Math.floor(n.y/120)}`;if(!buckets.has(k))buckets.set(k,[]);buckets.get(k).push(n);});
      nodes.forEach(n=>{const gx=Math.floor(n.x/120),gy=Math.floor(n.y/120);for(let dx=-1;dx<=1;dx++)for(let dy=-1;dy<=1;dy++)for(const q of buckets.get(`${gx+dx},${gy+dy}`)||[]){if(n===q)continue;const x=n.x-q.x,y=n.y-q.y,d=Math.max(20,Math.hypot(x,y));n.vx+=x/d*1600/(d*d)*alpha;n.vy+=y/d*1600/(d*d)*alpha;}});
      edges.forEach(([a,b])=>{const dx=b.x-a.x,dy=b.y-a.y,d=Math.hypot(dx,dy)||1,f=(d-100)*.025*alpha;a.vx+=dx/d*f;a.vy+=dy/d*f;b.vx-=dx/d*f;b.vy-=dy/d*f;});
      nodes.forEach(n=>{n.vx-=n.x*.0008*alpha;n.vy-=n.y*.0008*alpha;n.x+=n.vx;n.y+=n.vy;n.vx*=.7;n.vy*=.7;});ticks++;}
    for(let i=0;i<90;i++)step();fit();search();
  }
  function search(){const q=$('#graph-search').value.trim().toLowerCase(),el=$('#graph-results');el.replaceChildren();if(!q)return;const hits=[...all.values()].filter(n=>(n.title+' '+n.id).toLowerCase().includes(q)).slice(0,12);if(!hits.length)el.textContent='No matching notes';for(const n of hits){const b=document.createElement('button');b.className='graph-neighbor';b.textContent=n.title||n.id;b.onclick=()=>focusNode(n.id);el.append(b);}}
  function zoom(f,x=w/2,y=h/2){const z=Math.min(5,Math.max(.05,camera.z*f)),ratio=z/camera.z;camera.x=x-(x-camera.x)*ratio;camera.y=y-(y-camera.y)*ratio;camera.z=z;draw();}
  const pointers=new Map();let moved=false,start=null,pinch=null;
  function point(e){const r=cv.getBoundingClientRect();return{x:e.clientX-r.left,y:e.clientY-r.top};}
  cv.onpointerdown=e=>{cancelAnimationFrame(frame);cv.setPointerCapture(e.pointerId);pointers.set(e.pointerId,point(e));start=point(e);moved=false;pinch=null;};
  cv.onpointermove=e=>{const p=point(e);if(!pointers.has(e.pointerId)){let best=20,hit=null;nodes.forEach(n=>{const d=Math.hypot(n.x*camera.z+camera.x-p.x,n.y*camera.z+camera.y-p.y);if(d<best){best=d;hit=n.id;}});hover=hit;draw();return;}const old=pointers.get(e.pointerId);pointers.set(e.pointerId,p);if(pointers.size===2){const[a,b]=[...pointers.values()],d=Math.hypot(a.x-b.x,a.y-b.y);if(pinch)zoom(d/pinch,(a.x+b.x)/2,(a.y+b.y)/2);pinch=d;moved=true;}else{camera.x+=p.x-old.x;camera.y+=p.y-old.y;if(Math.hypot(p.x-start.x,p.y-start.y)>4)moved=true;draw();}};
  cv.onpointerup=e=>{const p=point(e);pointers.delete(e.pointerId);if(!moved){let best=24,hit=null;nodes.forEach(n=>{const d=Math.hypot(n.x*camera.z+camera.x-p.x,n.y*camera.z+camera.y-p.y);if(d<best){best=d;hit=n.id;}});if(hit)select(hit);}pinch=null;};
  cv.onpointercancel=e=>{pointers.delete(e.pointerId);moved=true;pinch=null;};
  const wheel=e=>{e.preventDefault();const p=point(e);zoom(Math.exp(-e.deltaY*.002),p.x,p.y);};cv.addEventListener('wheel',wheel,{passive:false});
  $('#graph-in').onclick=()=>zoom(1.3);$('#graph-out').onclick=()=>zoom(1/1.3);$('#graph-fit').onclick=fit;$('#graph-scope').onchange=rebuild;$('#graph-search').oninput=search;
  cv.onkeydown=e=>{if(e.key==='+'||e.key==='=')zoom(1.3);else if(e.key==='-')zoom(1/1.3);else if(e.key==='Home')fit();else return;e.preventDefault();};
  const observer=new ResizeObserver(()=>{const r=cv.getBoundingClientRect();w=r.width;h=r.height;const dpr=Math.min(devicePixelRatio||1,2);cv.width=w*dpr;cv.height=h*dpr;ctx.setTransform(dpr,0,0,dpr,0,0);fit();});observer.observe(cv);
  const rect=cv.getBoundingClientRect();w=rect.width;h=rect.height;const dpr=Math.min(devicePixelRatio||1,2);cv.width=w*dpr;cv.height=h*dpr;ctx.setTransform(dpr,0,0,dpr,0,0);rebuild();
  dispose=()=>{observer.disconnect();cancelAnimationFrame(frame);cv.removeEventListener('wheel',wheel);cv.onpointerdown=cv.onpointermove=cv.onpointerup=cv.onpointercancel=cv.onkeydown=null;};
}
