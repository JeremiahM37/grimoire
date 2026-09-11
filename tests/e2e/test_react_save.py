"""Data-preservation checks against the actual React editor and note API."""

import re

import pytest
from playwright.sync_api import expect


@pytest.fixture
def page(browser):
    # Intercept the browser transport to hold a PUT in flight; service-worker
    # requests bypass Playwright page routes and have their own offline suite.
    context=browser.new_context(service_workers="block")
    context.add_init_script("localStorage.setItem('grimoire-editor-mode','classic')")
    yield context.new_page()
    context.close()


def create(page, server, title):
    response = page.request.post(server+'/api/notes', data={'title':title,'body':'original'})
    assert response.ok
    return response.json()['path']


def test_delayed_save_keeps_newer_typing_and_serializes_requests(page, server):
    path=create(page,server,'Delayed React save')
    page.goto(server+'/#'+path)
    editor=page.locator('#content');expect(editor).to_have_value('original\n')
    held=[];bodies=[]
    def intercept(route):
        if route.request.method!='PUT':return route.continue_()
        bodies.append(route.request.post_data_json['body'])
        if len(bodies)==1:held.append(route)
        else:route.continue_()
    page.route('**/api/notes/'+path,intercept)
    editor.fill('first version')
    for _ in range(60):
        if held:break
        page.wait_for_timeout(100)
    assert len(held)==1, (editor.input_value(),page.locator('#save-state').inner_text(),page.locator('[role=alert]').all_text_contents(),bodies)
    editor.fill('newer typing stays')
    response=held[0].fetch();held[0].fulfill(response=response)
    expect(editor).to_have_value(re.compile(r'^newer typing stays\n?$'))
    expect(page.locator('#save-state')).to_have_text('saved')
    assert page.request.get(server+'/api/notes/'+path).json()['body'].rstrip('\n')=='newer typing stays'
    assert bodies==['first version','newer typing stays']


def test_failed_save_keeps_latest_draft_and_blocks_navigation(page, server):
    path=create(page,server,'Failed React save');other=create(page,server,'Other React save')
    page.goto(server+'/#'+path)
    editor=page.locator('#content');expect(editor).to_have_value('original\n')
    page.route('**/api/notes/'+path,lambda route:route.fulfill(status=503,json={'detail':'offline'}) if route.request.method=='PUT' else route.continue_())
    editor.fill('valuable unsaved text')
    expect(page.locator('#save-state')).to_contain_text('failed')
    page.locator('.note-row').filter(has_text='Other React save').click()
    expect(editor).to_have_value('valuable unsaved text')
    assert page.evaluate('(path)=>JSON.parse(localStorage.getItem("grimoire-react-draft:"+path)).body',path)=='valuable unsaved text'
    assert page.request.get(server+'/api/notes/'+other).json()['body']=='original\n'


def test_delete_stays_deleted_until_explicit_undo(page, server):
    path=create(page,server,'Explicit React undo')
    page.goto(server+'/#'+path);expect(page.locator('#content')).to_have_value('original\n')
    page.locator('#delete-note').click()
    page.locator('.form-panel button[type=submit]').click()
    expect(page.get_by_role('button',name='Undo delete',exact=True)).to_be_visible()
    page.wait_for_timeout(5300)
    assert page.request.get(server+'/api/notes/'+path).status==404
    page.get_by_role('button',name='Undo delete',exact=True).click()
    expect(page.locator('#content')).to_have_value('original\n')


def test_encrypted_note_failure_never_persists_plaintext_draft(page, server):
    for endpoint in ['init','unlock']:
        response=page.request.post(server+'/api/vault/'+endpoint,data={'passphrase':'mypassphrase123'})
        if response.ok:break
    assert response.ok
    path=create(page,server,'Encrypted React draft')
    assert page.request.post(server+'/api/notes/'+path+'/encrypt').ok
    page.goto(server+'/#'+path)
    editor=page.locator('#content');expect(editor).to_have_value('original\n')
    page.route('**/api/notes/'+path,lambda route:route.fulfill(status=503,json={'detail':'offline'}) if route.request.method=='PUT' else route.continue_())
    editor.fill('fixture confidential content')
    expect(page.locator('#save-state')).to_contain_text('failed')
    assert page.evaluate('(path)=>localStorage.getItem("grimoire-react-draft:"+path)',path) is None
    assert not page.evaluate('Object.values(localStorage).some(value=>value.includes("fixture confidential content"))')
