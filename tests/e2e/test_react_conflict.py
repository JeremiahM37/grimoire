"""A stale editor must not overwrite what an agent wrote, and the owner's inbox shows up."""

import pytest
from playwright.sync_api import expect


@pytest.fixture
def page(browser):
    context=browser.new_context(service_workers="block")
    context.add_init_script("localStorage.setItem('grimoire-editor-mode','classic')")
    yield context.new_page()
    context.close()


def open_note(page, server, title):
    response=page.request.post(server+'/api/notes', data={'title':title,'body':'original'})
    assert response.ok
    path=response.json()['path']
    page.goto(server+'/#'+path)
    expect(page.locator('#content')).to_have_value('original\n')
    return path


def agent_writes(page, server, path, body):
    # an agent's write, with no base version: the server takes it as before
    assert page.request.put(server+'/api/notes/'+path, data={'body':body}).ok


@pytest.mark.parametrize('choice,expected', [
    ('Keep both', ['agent line', 'my edit']),
    ('Keep mine', ['my edit']),
    ('Reload theirs', ['agent line']),
])
def test_stale_save_is_refused_and_the_owner_chooses(page, server, choice, expected):
    path=open_note(page, server, 'Conflict '+choice)
    agent_writes(page, server, path, 'original\nagent line\n')
    page.locator('#content').fill('original\nmy edit')
    bar=page.locator('.note-conflict-bar')
    expect(bar).to_be_visible()
    # nothing was overwritten while the bar is up
    assert 'agent line' in page.request.get(server+'/api/notes/'+path).json()['body']
    bar.get_by_role('button', name=choice).click()
    expect(bar).to_be_hidden()
    if choice=='Reload theirs':
        expect(page.locator('#content')).to_have_value('original\nagent line\n')
    else:
        expect(page.locator('#save-state')).to_have_text('saved')
    body=page.request.get(server+'/api/notes/'+path).json()['body']
    for text in expected:
        assert text in body, body
    if choice=='Keep mine':
        assert 'agent line' not in body


def test_inbox_is_readable_without_the_admin_credential(page, server):
    response=page.request.get(server+'/api/inbox')
    assert response.ok
    data=response.json()
    assert isinstance(data['items'], list) and set(data['counts'])>={'approvals','credentials','total'}
