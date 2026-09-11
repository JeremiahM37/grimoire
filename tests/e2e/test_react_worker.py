"""The built worker supports the console offline without caching note content."""
import pytest
from playwright.sync_api import expect


@pytest.fixture
def page(browser):
    context = browser.new_context(service_workers="allow")
    yield context.new_page()
    context.close()


def test_react_worker_offline_shell_and_private_content_exclusion(page, server):
    page.goto(server)
    expect(page.locator('#app')).to_be_visible()
    for _ in range(100):
        if page.evaluate('navigator.serviceWorker.controller !== null'):break
        page.wait_for_timeout(100)
    else: raise AssertionError('React service worker did not take control')
    response = page.request.post(server + '/api/notes', data={'title': 'Worker private export', 'body': 'NeverCacheThisPrivateBody'})
    assert response.ok
    path = response.json()['path']
    exported = page.evaluate("async path => { const r = await fetch('/notes/'+path+'/export.html'); return {status:r.status,body:await r.text()}; }", path)
    assert exported['status'] == 200
    assert 'NeverCacheThisPrivateBody' in exported['body']
    created = page.evaluate("async()=>{const r=await fetch('/api/notes',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:'Worker POST reaches server',body:'actual write'})});return {status:r.status,note:await r.json()};}")
    assert created['status'] in [200,201]
    assert page.request.get(server+'/api/notes/'+created['note']['path']).ok
    cache_urls = page.evaluate("async()=>{const names=(await caches.keys()).filter(n=>n.startsWith('grimoire-react-'));return (await Promise.all(names.map(async n=>(await(await caches.open(n)).keys()).map(r=>r.url)))).flat();}")
    assert any('/assets/' in url and url.endswith('.js') for url in cache_urls)
    assert any('/vendor/editor.js' in url for url in cache_urls)
    assert not any('/api/' in url or '/notes/' in url for url in cache_urls)
    page.context.set_offline(True)
    page.goto(server)
    expect(page.locator('#app')).to_be_visible()
    expect(page.locator('#new-note')).to_be_visible()
