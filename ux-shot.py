"""ux-shot.py OUT.png [--mobile] [--dark] [--hash note.md] [--click SEL]... [--js CODE]
Screenshots the scratch UX server (:9131). Run with ~/projects/grimoire/.venv/bin/python."""
import argparse
from playwright.sync_api import sync_playwright
a=argparse.ArgumentParser();a.add_argument('out');a.add_argument('--mobile',action='store_true');a.add_argument('--dark',action='store_true')
a.add_argument('--hash',default='');a.add_argument('--click',action='append',default=[]);a.add_argument('--js',default='');a.add_argument('--url',default='http://127.0.0.1:'+__import__('os').environ.get('UX_PORT','9131')+'/')
o=a.parse_args()
with sync_playwright() as p:
    b=p.chromium.launch()
    ctx=b.new_context(viewport={'width':390,'height':844} if o.mobile else {'width':1440,'height':900},color_scheme='dark' if o.dark else 'light',has_touch=o.mobile,is_mobile=o.mobile)
    pg=ctx.new_page(); errs=[]
    pg.on('pageerror',lambda e:errs.append(str(e)))
    pg.goto(o.url+('#'+o.hash if o.hash else '')); pg.wait_for_timeout(1500)
    for sel in o.click:
        try: pg.locator(sel).first.click(timeout=4000); pg.wait_for_timeout(900)
        except Exception as e: print('click failed',sel,str(e).splitlines()[0])
    if o.js: print(pg.evaluate(o.js))
    pg.screenshot(path=o.out); print('saved',o.out, 'pageerrors:',errs or 'none')
    b.close()
