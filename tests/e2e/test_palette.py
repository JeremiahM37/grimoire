"""Command palette: recents first, every note reachable by search, shortcuts and layout."""
from urllib.parse import quote

from conftest import DESKTOP, PHONE
from playwright.sync_api import expect


def _note(page, server, path, title):
    created = page.request.post(server + '/api/notes', data={'path': path, 'body': f'# {title}\n\nPalette body.\n'})
    assert created.status == 201, created.text()


def _open_note(page, server, path):
    page.goto(server + '/#' + quote(path, safe=''))
    page.wait_for_selector('body[data-ready]', timeout=10000)


def _palette(page):
    page.keyboard.press('Control+k')
    expect(page.locator('#palette')).to_be_visible()


def test_empty_palette_lists_the_previous_note_first_and_enter_switches_back(page, server):
    earlier, current = 'palette-recent-alpha.md', 'palette-recent-beta.md'
    _note(page, server, earlier, 'Palette Recent Alpha')
    _note(page, server, current, 'Palette Recent Beta')
    _open_note(page, server, earlier)
    expect(page.locator('#title')).to_have_value('Palette Recent Alpha', timeout=8000)
    _open_note(page, server, current)
    expect(page.locator('#title')).to_have_value('Palette Recent Beta', timeout=8000)
    _palette(page)
    expect(page.locator('#palette-list .pal-heading').first).to_have_text('Recent')
    first = page.locator('#palette-list .pal-item').first
    expect(first).to_have_attribute('data-kind', 'note')
    # the note you are in is not offered: the top row is the one you came from
    expect(first).to_have_attribute('data-value', earlier)
    expect(page.locator(f'#palette-list .pal-item[data-value="{current}"]')).to_have_count(0)
    expect(page.locator('#palette-list .pal-heading', has_text='Commands')).to_have_count(1)
    page.keyboard.press('Enter')
    expect(page.locator('#title')).to_have_value('Palette Recent Alpha', timeout=8000)


def test_typing_part_of_a_note_outside_the_first_twenty_opens_it(page, server):
    target = 'palette-zz-deep-lookup-target.md'
    _note(page, server, target, 'Deep Lookup Target')
    for index in range(25):
        _note(page, server, f'palette-filler-{index:02d}.md', f'Palette filler {index:02d}')
    listed = [row['path'] for row in page.request.get(server + '/api/notes').json()]
    assert listed.index(target) >= 20, 'the target must sit outside the first twenty notes'
    page.goto(server)
    page.wait_for_selector('body[data-ready]', timeout=10000)
    _palette(page)
    page.fill('#palette-input', 'deep lookup')
    expect(page.locator('#palette-list .pal-item.sel')).to_have_attribute('data-value', target)
    page.keyboard.press('Enter')
    expect(page.locator('#palette')).to_be_hidden()
    expect(page.locator('#title')).to_have_value('Deep Lookup Target', timeout=5000)


def test_angle_bracket_shows_only_commands(page, server):
    page.goto(server)
    page.wait_for_selector('body[data-ready]', timeout=10000)
    _palette(page)
    page.fill('#palette-input', '>graph')
    expect(page.locator('#palette-list .pal-item').first).to_have_attribute('data-value', 'Open graph view')
    expect(page.locator('#palette-list .pal-item[data-kind=note]')).to_have_count(0)
    expect(page.locator('#palette-list .pal-item[data-kind=command] kbd')).to_have_text('Ctrl+G')


def test_no_match_offers_create_note_and_creates_it(page, server):
    page.goto(server)
    page.wait_for_selector('body[data-ready]', timeout=10000)
    _palette(page)
    page.fill('#palette-input', 'Zzq Created Probe')
    row = page.locator('#palette-list .pal-item[data-kind=create]')
    expect(row).to_have_count(1)
    expect(row).to_contain_text('Create note "Zzq Created Probe"')
    page.keyboard.press('Enter')
    expect(page.locator('#palette')).to_be_hidden()
    expect(page.locator('#title')).to_have_value('Zzq Created Probe', timeout=8000)


def test_arrow_keys_and_enter_run_the_selected_command(page, server):
    page.goto(server)
    page.wait_for_selector('body[data-ready]', timeout=10000)
    _palette(page)
    page.fill('#palette-input', '>')
    for _ in range(3):
        page.keyboard.press('ArrowDown')
    expect(page.locator('#palette-list .pal-item.sel')).to_have_attribute('data-value', 'Open graph view')
    page.keyboard.press('Enter')
    expect(page.locator('#graph-modal')).to_be_visible(timeout=5000)


def test_ctrl_o_and_ctrl_p_open_the_palette(page, server):
    page.goto(server)
    page.wait_for_selector('body[data-ready]', timeout=10000)
    page.keyboard.press('Control+o')
    expect(page.locator('#palette')).to_be_visible()
    page.keyboard.press('Escape')
    expect(page.locator('#palette')).to_be_hidden()
    page.keyboard.press('Control+p')
    expect(page.locator('#palette-input')).to_have_value('>')
    expect(page.locator('#palette-list .pal-item[data-kind=note]')).to_have_count(0)


def test_palette_fits_the_viewport_on_desktop_and_phone(page, server):
    for viewport in (DESKTOP, PHONE):
        page.set_viewport_size(viewport)
        page.goto(server)
        page.wait_for_selector('body[data-ready]', timeout=10000)
        _palette(page)
        # the sheet slides in; measure once its entrance animation has finished, not mid-flight
        page.locator('#palette .modal-box').evaluate('el => Promise.all(el.getAnimations().map(a => a.finished))')
        box = page.locator('#palette .modal-box').bounding_box()
        assert box is not None
        assert box['y'] >= 0 and box['y'] + box['height'] <= viewport['height'] + 1, viewport
        assert box['x'] >= 0 and box['x'] + box['width'] <= viewport['width'] + 1, viewport
        page.keyboard.press('Escape')
        expect(page.locator('#palette')).to_be_hidden()
