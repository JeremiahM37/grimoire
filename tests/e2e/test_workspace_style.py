"""Regression: semantic note buttons must not inherit native bright borders."""
import pytest
from playwright.sync_api import expect


@pytest.mark.parametrize('theme', ['light', 'dark'])
def test_note_rows_are_contained_and_keyboard_accessible(page, server, theme):
    title = 'Workspace visual regression with a deliberately long note title ' * 3
    path = f'workspace-{theme}.md'
    response = page.request.post(server + '/api/notes', data={
        'path': path, 'title': title, 'body': '# Workspace\n\nA quiet place to write.',
    })
    assert response.ok
    page.add_init_script(f"localStorage.setItem('grimoire-theme', '{theme}')")
    page.goto(server + '/#' + path)
    page.wait_for_selector('body[data-ready]')
    row = page.locator(f'.note-row[data-path="{path}"]')
    expect(row).to_have_attribute('aria-current', 'page')
    styles = row.evaluate('''el => {
      const s = getComputedStyle(el), box = el.getBoundingClientRect();
      const list = document.querySelector('#note-list').getBoundingClientRect();
      return {border: s.borderTopWidth, shadow: s.boxShadow,
        contained: box.left >= list.left && box.right <= list.right,
        titleTruncated: el.querySelector('.t').scrollWidth > el.querySelector('.t').clientWidth};
    }''')
    assert styles == {'border': '0px', 'shadow': 'none', 'contained': True, 'titleTruncated': True}
    row.focus()
    expect(row).to_be_focused()
    assert row.evaluate("el => getComputedStyle(el).outlineStyle") != 'none'
    page.set_viewport_size({'width': 390, 'height': 844})
    page.locator('#menu-open').click()
    page.locator('#search').fill(title[:30])
    row.click()
    expect(page.locator('#sidebar')).not_to_have_class('open')
    expect(page.locator('#title')).to_have_value(title.strip())
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
