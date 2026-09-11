"""AI replies must not mutate a different note or hide request failures."""
from playwright.sync_api import expect


def note(page, server, title):
    response = page.request.post(server + '/api/notes', data={'title': title, 'body': 'Original text'})
    assert response.ok
    return response.json()['path']


def test_late_action_does_not_edit_newly_opened_note(page, server):
    first = note(page, server, 'AI source')
    second = note(page, server, 'AI destination')
    held = []
    page.route('**/api/actions', lambda route: held.append(route))
    page.goto(server + '/#' + first)
    expect(page.locator('#title')).to_have_value('AI source')
    page.click('#ai-btn')
    page.click('#ai-menu [data-a=summarize]')
    for _ in range(50):
        if held:
            break
        page.wait_for_timeout(100)
    assert len(held) == 1
    page.evaluate('(path) => { location.hash = path; }', second)
    expect(page.locator('#title')).to_have_value('AI destination')
    held[0].fulfill(json={'result': 'A delayed AI summary'})
    expect(page.locator('[role=alert]')).to_contain_text('different note')
    expect(page.locator('#content')).not_to_have_value('A delayed AI summary')
    assert 'delayed' not in page.request.get(server + '/api/notes/' + second).json()['body']


def test_action_error_is_visible_and_note_unchanged(page, server):
    path = note(page, server, 'AI failure')
    page.route('**/api/actions', lambda route: route.fulfill(status=503, json={'detail': 'Reader offline'}))
    page.goto(server + '/#' + path)
    expect(page.locator('#title')).to_have_value('AI failure')
    page.click('#ai-btn')
    page.click('#ai-menu [data-a=expand]')
    expect(page.locator('[role=alert]')).to_contain_text('AI action failed')
    assert page.request.get(server + '/api/notes/' + path).json()['body'].strip() == 'Original text'
