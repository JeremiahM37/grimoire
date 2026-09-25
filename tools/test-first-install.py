#!/usr/bin/env python3
"""Fresh installed/Docker server acceptance. Uses only disposable vaults and tokens.

Local: --prefix /path/to/installed/prefix
Docker: --image grimoire:acceptance --url http://localhost:19132
GRIMOIRE_TEST_DOCKER may name a remote Docker command (shell-word syntax).
Requires Playwright/Chromium. No LLM provider is called.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import shlex
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def run(cmd, **kw):
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=180, **kw)
    if p.returncode:
        raise RuntimeError(f'{cmd[0]} failed: {p.stderr[-1500:]}')
    return p.stdout


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--prefix', type=Path)
    parser.add_argument('--image')
    parser.add_argument('--url', default='http://127.0.0.1:19132')
    args = parser.parse_args()
    assert bool(args.prefix) != bool(args.image), 'choose installed prefix OR Docker image'
    name = 'grimoire-first-run-' + secrets.token_hex(5)
    token = secrets.token_hex(32)
    docker = shlex.split(os.environ.get('GRIMOIRE_TEST_DOCKER', 'docker'))
    process = None
    local = tempfile.TemporaryDirectory(prefix=name)
    home = Path(local.name)
    env = {k: v for k, v in os.environ.items() if not k.startswith('GRIMOIRE_')}
    env.update(HOME=str(home), GRIMOIRE_VAULT=str(home/'vault'),
               GRIMOIRE_LOCAL_EMBED='off', GRIMOIRE_AUTH_TOKEN=token,
               GRIMOIRE_PORT='19132', GRIMOIRE_URL=args.url)

    def api(path, data=None, method=None):
        request = urllib.request.Request(args.url+'/api'+path,
                   data=None if data is None else json.dumps(data).encode(),
                   headers={'Authorization': 'Bearer '+token, 'Content-Type': 'application/json'},
                   method=method)
        with urllib.request.urlopen(request, timeout=20) as response:
            return json.load(response)

    def start():
        nonlocal process
        if args.image:
            run(docker+['run', '-d', '--name', name, '--memory', '384m', '--cpus', '1',
                        '-p', '19132:9111', '-v', name+':/vault',
                        '-e', 'GRIMOIRE_AUTH_TOKEN='+token, args.image])
        else:
            process = subprocess.Popen([str(args.prefix/'bin/grimoire'), 'serve'],
                        env=env, cwd=home, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(150):
            try:
                if api('/health')['ok']:
                    return
            except OSError:
                time.sleep(.2)
        raise AssertionError('server failed to start')

    def stop():
        nonlocal process
        if args.image:
            run(docker+['rm', '-f', name])
        elif process:
            process.terminate()
            process.wait(timeout=15)
            process = None

    try:
        if args.image:
            run(docker+['volume', 'create', name])
        start()
        try:
            urllib.request.urlopen(args.url+'/api/notes')
            raise AssertionError('API accepted unauthenticated visitor')
        except urllib.error.HTTPError as e:
            assert e.code == 401
        note = api('/notes', {'title': 'Fresh install kestrel', 'body': 'Copper gateway verification'})
        path = note['path']
        assert 'Copper' in api('/notes/'+path)['body']
        assert any(row['path'] == path for row in api('/search?q=kestrel'))
        # Real MCP process talking to this server, including authentication.
        requests = [
            {'jsonrpc':'2.0','id':1,'method':'initialize','params':{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'install-test','version':'1'}}},
            {'jsonrpc':'2.0','id':2,'method':'tools/call','params':{'name':'read_note','arguments':{'path':path}}},
        ]
        mcp_cmd = (docker+['exec', '-i', '-e', 'GRIMOIRE_AUTH_TOKEN='+token, name, 'grimoire-mcp']
                   if args.image else [str(args.prefix/'bin/grimoire-mcp')])
        output = run(mcp_cmd, env=env, input=''.join(json.dumps(r)+'\n' for r in requests))
        replies = [json.loads(line) for line in output.splitlines() if line.startswith('{')]
        read = next(r for r in replies if r.get('id') == 2)
        assert not read.get('error') and not read['result'].get('isError'), read
        assert 'Copper gateway' in json.dumps(read)
        from playwright.sync_api import sync_playwright, expect
        with sync_playwright() as pw:
            browser = pw.chromium.launch(headless=True)
            for width in (1280, 390):
                context = browser.new_context(viewport={'width':width,'height':844})
                context.add_init_script("localStorage.setItem('grimoire-editor-mode','classic')")
                page = context.new_page()
                page.goto(args.url)
                expect(page.get_by_role('heading', name='Sign in to Grimoire')).to_be_visible()
                page.get_by_label('Access token').fill('wrong-token')
                page.get_by_role('button', name='Connect').click()
                expect(page.get_by_role('status')).to_contain_text('not accepted')
                page.get_by_label('Access token').fill(token)
                page.get_by_role('button', name='Connect').click()
                expect(page.locator('.note-row').filter(has_text='Fresh install kestrel')).to_be_visible()
                page.goto(args.url+'/#'+path)
                editor = page.locator('#content')
                expect(editor).to_be_visible()
                editor.fill('Browser edited kestrel '+str(width))
                expect(page.locator('#save-state')).to_have_text('saved', timeout=10000)
                assert 'Browser edited kestrel '+str(width) in api('/notes/'+path)['body']
                page.reload()
                expect(editor).to_have_value('Browser edited kestrel '+str(width)+'\n')
                assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), width
                context.close()
            browser.close()
        print('PASS authenticated browser login/edit/reload at 390/1280px; API search; real MCP note retrieval', flush=True)
        if args.image:
            run(docker+['exec',name,'pdftotext','-v'])
        api('/settings', {'llm_model':'fixture-persisted-model'}, method='PUT')
        stop()
        start()
        assert 'Browser edited kestrel 390' in api('/notes/'+path)['body']
        assert any(row['path'] == path for row in api('/search?q=kestrel'))
        settings = api('/settings')
        assert 'fixture-persisted-model' in json.dumps(settings), settings
        print('PASS notes, search index and settings survive restart/container recreation', flush=True)
    finally:
        if args.image:
            subprocess.run(docker+['rm','-f',name], capture_output=True)
            subprocess.run(docker+['volume','rm',name], capture_output=True)
        elif process:
            stop()
        local.cleanup()


if __name__ == '__main__':
    main()
