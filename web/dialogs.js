/* Shared form panels and keyboard behavior for every modal surface. */
const focusable = 'button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])';
const visible = el => !el.classList.contains('hidden') && el.getClientRects().length;
let stack = [], lastFocus = document.activeElement, previousFocus = lastFocus;
document.addEventListener('focusin', () => { previousFocus = lastFocus; lastFocus = document.activeElement; });
function syncDialogs() {
  const current = [...document.querySelectorAll('.modal')].filter(visible);
  const closed = stack.filter(entry => !current.includes(entry.el));
  stack = stack.filter(entry => current.includes(entry.el));
  for (const el of current) if (!stack.some(entry => entry.el === el)) {
    const restore = el.contains(document.activeElement) ? previousFocus : document.activeElement;
    stack.push({el, restore});
    el.setAttribute('role', 'dialog'); el.setAttribute('aria-modal', 'true');
    if(!el.hasAttribute('aria-labelledby')&&!el.hasAttribute('aria-label'))el.setAttribute('aria-label',el.querySelector('.modal-head span')?.textContent || 'Dialog');
    el.tabIndex = -1;
    if (!el.contains(document.activeElement)) (el.querySelector(focusable) || el).focus();
  }
  if (closed.length && !current.some(el => el.contains(document.activeElement))) {
    const target = closed.at(-1).restore;
    if (target?.isConnected && target.getClientRects().length) target.focus();
    else if (stack.length) (stack.at(-1).el.querySelector(focusable) || stack.at(-1).el).focus();
    else (document.querySelector('.cm-content') || document.querySelector('#content'))?.focus();
  }
  document.body.classList.toggle('dialog-open', !!stack.length);
}
new MutationObserver(syncDialogs).observe(document.body, {subtree:true, attributes:true, attributeFilter:['class'], childList:true});
document.addEventListener('keydown', e => {
  syncDialogs(); const top = stack.at(-1)?.el; if (!top) return;
  if (e.key === 'Escape') {
    e.preventDefault(); e.stopImmediatePropagation();
    const close = top.querySelector('[data-panel-cancel], button[id$="-close"], .modal-head button.icon');
    if (close) close.click(); else top.classList.add('hidden');
  } else if (e.key === 'Tab') {
    const items = [...top.querySelectorAll(focusable)].filter(el => el.getClientRects().length);
    if (!items.length) { e.preventDefault(); top.focus(); return; }
    const first=items[0], last=items.at(-1), active=document.activeElement;
    if (!top.contains(active) || e.shiftKey && active===first || !e.shiftKey && active===last) {
      e.preventDefault(); (e.shiftKey ? last : first).focus();
    }
  }
}, true);
function viewportSize() {
  document.documentElement.style.setProperty('--visible-height', `${window.visualViewport?.height || innerHeight}px`);
  document.documentElement.style.setProperty('--visible-top', `${window.visualViewport?.offsetTop || 0}px`);
}
window.visualViewport?.addEventListener('resize', viewportSize);
window.visualViewport?.addEventListener('scroll', viewportSize);
addEventListener('resize', viewportSize); viewportSize();

export function formPanel({title, description='', fields=[], submit='Save'}) {
  return new Promise(resolve => {
    const modal=document.createElement('div'); modal.className='modal form-panel';
    const form=document.createElement('form'); form.className='modal-box';
    const header=document.createElement('header'); header.className='modal-head';
    const heading=document.createElement('span'); heading.textContent=title;
    header.append(heading); form.append(header);
    const body=document.createElement('div'); body.className='new-note-fields';
    if(description){const p=document.createElement('p');p.textContent=description;p.className='panel-description';body.append(p);}
    const inputs=new Map();
    for(const field of fields){
      const label=document.createElement('label'); label.textContent=field.label;
      const input=document.createElement(field.options?'select':field.multiline?'textarea':'input');
      input.name=field.name; input.setAttribute('aria-label',field.label);
      if(field.options)for(const option of field.options){const o=document.createElement('option');o.value=option.value;o.textContent=option.label;input.append(o);}
      else if(!field.multiline)input.type=field.type || 'text';
      input.value=field.value || ''; input.required=field.required ?? true;
      if(field.minLength)input.minLength=field.minLength;
      input.autocomplete=field.type==='password'?'off':'on';
      label.append(input);body.append(label);inputs.set(field.name,input);
    }
    const footer=document.createElement('footer');footer.className='new-note-actions';
    const cancel=document.createElement('button');cancel.type='button';cancel.className='btn';cancel.textContent='Cancel';cancel.dataset.panelCancel='';
    const accept=document.createElement('button');accept.type='submit';accept.className='btn primary';accept.textContent=submit;
    footer.append(cancel,accept);form.append(body,footer);modal.append(form);
    let settled=false;
    function finish(value){if(settled)return;settled=true;for(const input of inputs.values())input.value='';modal.remove();resolve(value);}
    cancel.onclick=()=>finish(null);
    modal.onclick=e=>{if(e.target===modal)finish(null);};
    form.onsubmit=e=>{e.preventDefault();finish(Object.fromEntries([...inputs].map(([name,input])=>[name,input.value])));};
    document.body.append(modal);syncDialogs();
    const first=inputs.values().next().value;
    if(first){first.focus();if(first.type!=='password'&&first.select)first.select();}else cancel.focus();
  });
}
export async function askText(title, value='', options={}) {
  const presets=[[/Rename note/, 'Rename note', 'New title', 'Rename'],[/extracted note/, 'Extract selection', 'New note title', 'Extract'],[/Merge this note/, 'Merge notes', 'Destination note title', 'Continue'],[/Template name/, 'Save as template', 'Template name', 'Save template'],[/Title for the new note/, 'Create from template', 'Note title', 'Create note'],[/What would the agent/, 'Inspect note context', 'Question', 'Inspect'],[/Plugin name/, 'Create plugin', 'Plugin name (lowercase, hyphens)', 'Create plugin'],[/New canvas name/, 'New canvas', 'Canvas name', 'Create canvas'],[/Card text/, 'Edit canvas card', 'Card text', 'Save card']];
  const preset=presets.find(([pattern])=>pattern.test(title));
  if(preset){title=preset[1];options={label:preset[2],submit:preset[3],multiline:preset[0].source==='Card text',...options};}
  const result=await formPanel({title, submit:options.submit || 'Continue', fields:[{name:'value',label:options.label || title.replace(/:$/, ''),value,required:!options.optional,type:/passphrase/i.test(title)?'password':'text',multiline:/Card text/i.test(title),...options}]});
  return result?.value ?? null;
}
export async function confirmAction(description) {
  const verb=/delete forever/i.test(description)?'Delete forever':/trash/i.test(description)?'Move to trash':/restore/i.test(description)?'Restore':/remove/i.test(description)?'Remove':/enable/i.test(description)?'Enable':/create/i.test(description)?'Create':'Confirm';
  return (await formPanel({title:verb,description,submit:verb}))!==null;
}
