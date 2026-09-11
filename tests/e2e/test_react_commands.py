"""Retained command-palette entry points execute their real flows."""
import io
import zipfile

from playwright.sync_api import expect


def command(page, title):
    page.keyboard.press('Control+k')
    page.fill('#palette-input', title)
    page.locator('#palette-list .pal-item').filter(has_text=title).first.click()


def test_vault_archive_import_and_export_from_palette(page, server):
    page.goto(server)
    page.wait_for_selector('body[data-ready]')
    archive = io.BytesIO()
    with zipfile.ZipFile(archive, 'w') as zipped:
        zipped.writestr('imported-command.md', '# Imported command\n\nArchive body')
    with page.expect_file_chooser() as chooser:
        command(page, 'Import vault from .zip')
    chooser.value.set_files({'name': 'notes.zip', 'mimeType': 'application/zip', 'buffer': archive.getvalue()})
    expect(page.locator('[role=alert]')).to_contain_text('Imported 1 files')
    assert 'Archive body' in page.request.get(server + '/api/notes/imported-command.md').json()['body']
    with page.expect_download() as download:
        command(page, 'Export whole vault')
    with zipfile.ZipFile(download.value.path()) as exported:
        assert 'imported-command.md' in exported.namelist()


def test_find_random_and_sync_commands(page, server):
    page.request.post(server + '/api/notes', data={'title': 'Command target', 'body': 'Search inside this note'})
    page.goto(server + '/#command-target.md')
    expect(page.locator('#title')).to_have_value('Command target')
    command(page, 'Find & replace in note')
    expect(page.locator('#find-input')).to_be_focused()
    page.click('#find-close')
    with page.expect_response('**/api/notes/random') as selected:
        command(page, 'Open random note')
    path = selected.value.json()['path']
    expect(page).to_have_url(server + '/#' + path)
    requests = []
    def sync(route):
        requests.append(route.request.method)
        route.fulfill(json={'pulled': 2, 'pushed': 3, 'conflicts': 0})
    page.route('**/api/sync/now', sync)
    command(page, 'Sync now')
    expect(page.locator('[role=alert]')).to_contain_text('2 pulled, 3 pushed')
    assert requests == ['POST']
