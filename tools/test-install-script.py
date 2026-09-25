#!/usr/bin/env python3
"""Exercise install.sh against a real candidate archive and failed upgrades."""
import argparse
import functools
import hashlib
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('archive', type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix='grimoire-installer-') as temp:
        temp = Path(temp)
        downloads = temp/'downloads'
        downloads.mkdir()
        archive = downloads/args.archive.name
        shutil.copy2(args.archive, archive)
        original = archive.read_bytes()
        checksums = downloads/'checksums.txt'

        def checksum():
            checksums.write_text(hashlib.sha256(archive.read_bytes()).hexdigest()+'  '+archive.name+'\n')

        checksum()

        class Quiet(SimpleHTTPRequestHandler):
            def log_message(self, *args):
                pass
        server = ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(Quiet, directory=str(downloads)))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        env = {k:v for k,v in os.environ.items() if not k.startswith('GRIMOIRE_')}
        home = temp/'home with spaces'
        home.mkdir()
        env.update(HOME=str(home), GRIMOIRE_RELEASE_BASE=f'http://127.0.0.1:{server.server_port}')
        binary = home/'.local/bin/grimoire'
        share = home/'.local/share/grimoire'

        def install(ok=True):
            p = subprocess.run(['sh',str(root/'install.sh')],env=env,cwd=temp,capture_output=True,text=True,timeout=60)
            assert (p.returncode == 0) == ok, p.stdout+p.stderr
        try:
            install()
            assert binary.is_file() and (share/'web/index.html').is_file()
            version = subprocess.check_output([binary,'version'],text=True)
            (share/'upgrade-preservation-proof').write_text('retained')
            install()
            assert any((p/'upgrade-preservation-proof').read_text() == 'retained'
                       for p in share.parent.glob('grimoire.previous.*') if (p/'upgrade-preservation-proof').exists())
            archive.write_bytes(b'corrupted download')
            install(ok=False)
            assert subprocess.check_output([binary,'version'],text=True) == version
            checksum()
            install(ok=False)
            assert subprocess.check_output([binary,'version'],text=True) == version
            archive.write_bytes(original)
            print('PASS personal install without sudo, paths with spaces, repeat upgrade preserves previous tree, checksum and corrupt-archive failures preserve working binary')
        finally:
            server.shutdown()
            server.server_close()


if __name__ == '__main__':
    main()
