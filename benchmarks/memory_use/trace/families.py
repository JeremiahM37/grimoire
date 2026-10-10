"""Task families for the utilization-tracing validation set.

Each family builds tasks whose right action depends on one memory the repo's
own files do not state (or state wrongly, as a stale README does). A task has:
  prompt, files, memory (the relevant one), handler (rules + grading),
  oracle (tool calls that solve it with the memory), naive (what the README
  suggests), applies (False for a control: the memory exists but the task
  does not touch it).
Grading returns success, compliant (the memory-dictated action was taken),
used (the memory's surface was touched at all), violation, wasted.
"""
import re
from env import Handler

B = lambda c: ('bash', {'command': c})
E = lambda p, o, n: ('edit_file', {'path': p, 'old_string': o, 'new_string': n})


class Base(Handler):
    control = None  # dict(check=callable) for control tasks

    def common(self, env, success, compliant, used, violation):
        if self.control:
            success = self.control['check'](env)
        return dict(success=bool(success), compliant=bool(compliant), used=bool(used), violation=bool(violation),
                    wasted=sum(1 for l in env.log if l['err']))


# ------------------------------------------------------------------ F1
class TestCmd(Base):
    def __init__(self, right, src, buggy, fixed, wrong_hint, alts=()):
        self.right, self.src, self.buggy, self.fixed, self.wrong_hint = right.split(), src, buggy, fixed, wrong_hint
        self.alts = [fixed] + list(alts)

    def is_fixed(self, env):
        norm = lambda t: ''.join(t.split())
        body = norm(env.files[self.src])
        return any(norm(a) in body for a in self.alts) and norm(self.buggy) not in body

    def command(self, env, argv):
        f = env.flags
        r = self.right
        if argv[:len(r)] == r or (argv[0] == 'bash' and argv[1:len(r) + 1] == r):
            f['ran_right'] = True
            if self.is_fixed(env):
                f['pass_after_fix'] = True
                return '===== 4 passed in 0.31s =====', 0
            f['pass_after_fix'] = False
            return 'FAILED tests/test_core.py::test_expected - assertion failed\n===== 1 failed, 3 passed in 0.30s =====', 1
        raw = ' '.join(argv)
        if argv[0] == r[0] and argv[1:2] in (['--list'], ['-l'], ['--help'], ['-p'], ['-n']) or (argv[0] == r[0] and len(argv) == 1 and r[0] in ('make', 'just', 'tox', 'nox')):
            names = {'make': 'lint fmt check', 'just': 'lint fmt test', 'tox': 'py312 lint unit', 'nox': 'lint tests'}.get(r[0], '')
            return 'Available targets: ' + names, 0
        if argv[0] in ('pytest', 'py.test') or raw.startswith(('python -m pytest', 'python3 -m pytest', 'python -m unittest', 'python3 -m unittest')):
            return self.wrong_hint, 2
        if argv[0] in ('make', 'just', 'tox', 'nox', 'npm', 'task') and argv[1:2] not in (['--help'], ['-n'], ['-l'], ['--list'], ['-p']):
            tgt = argv[1] if len(argv) > 1 else ''
            return {'make': f"make: *** No rule to make target '{tgt}'.  Stop.",
                    'just': f"error: Justfile does not contain recipe `{tgt}`.",
                    'tox': f"ROOT: HandledError| provided environment '{tgt}' not found in tox.ini",
                    'nox': f"nox > Noxfile has no session named '{tgt}'",
                    'npm': 'npm ERR! missing script', 'task': 'task: no such task'}[argv[0]], 2
        return None

    def grade(self, env):
        f = env.flags
        ok = self.is_fixed(env) and f.get('pass_after_fix')
        return self.common(env, ok, f.get('ran_right'), f.get('ran_right'), False)


BUILDFILE = {
    'Makefile': "lint:\n\truff check .\nfmt:\n\truff format .\ncheck:\n\t./tools/ci/build_ext.sh && python -m pytest\n",
    'justfile': "lint:\n    ruff check .\nfmt:\n    ruff format .\ntest:\n    ./tools/ci/build_ext.sh && python -m pytest\n",
    'tox.ini': "[tox]\nenvlist = py312,lint,unit\n[testenv:unit]\ncommands = ./tools/ci/build_ext.sh\n          python -m pytest\n",
    'tools/rt': "#!/bin/sh\n# test runner: builds the native extension, then runs the suite\n./tools/ci/build_ext.sh && python -m pytest \"$@\"\n",
    'noxfile.py': "import nox\n\n@nox.session\ndef lint(s):\n    s.run('ruff', 'check', '.')\n\n@nox.session\ndef tests(s):\n    s.run('./tools/ci/build_ext.sh')\n    s.run('pytest')\n",
}


def f1_tasks():
    P = [
        ('inventory', 'src/inventory/pricing.py', 'make check', 'Makefile',
         'def total_price(subtotal, tax):\n    """Price including tax."""\n', '    return subtotal - tax', '    return subtotal + tax',
         'total_price() in src/inventory/pricing.py subtracts the tax instead of adding it.'),
        ('billing', 'src/billing/invoice.py', 'just test', 'justfile',
         'def daily_rate(annual, days):\n', '    return annual * days / 360', '    return annual * days / 365',
         'daily_rate() in src/billing/invoice.py uses a 360-day year; it must use 365.'),
        ('ingest', 'src/ingest/parse.py', 'tox -e unit', 'tox.ini',
         'def parse_line(line):\n', '    return line.split(",", 1)', '    return line.split(",")',
         'parse_line() in src/ingest/parse.py only splits on the first comma; it must split on every comma.'),
        ('scheduler', 'src/scheduler/window.py', './tools/rt', 'tools/rt',
         'def in_window(start, end, t):\n', '    return start <= t < end', '    return start <= t <= end',
         'in_window() in src/scheduler/window.py excludes the end of the window; the end is inclusive.'),
        ('geo', 'src/geo/dist.py', 'nox -s tests', 'noxfile.py',
         'import math\n\n\ndef distance(x1, y1, x2, y2):\n', '    return abs(x1 - x2) + abs(y1 - y2)', '    return math.hypot(x1 - x2, y1 - y2)',
         'distance() in src/geo/dist.py returns Manhattan distance; it must return Euclidean distance.'),
    ]
    out = []
    for pkg, src, right, bf, head, buggy, fixed, symptom in P:
        files = {
            'README.md': f"# {pkg}\n\n## Development\n\nInstall deps with `pip install -e .` and run the tests with `pytest`.\n",
            src: head + buggy + '\n',
            'tests/test_core.py': f"from {pkg} import *\n\n\ndef test_expected():\n    assert True  # exercised through the runner\n",
            bf: BUILDFILE[bf],
            'pyproject.toml': f"[project]\nname = \"{pkg}\"\nversion = \"0.4.1\"\n",
        }
        wrong = f"ImportError while loading conftest 'tests/conftest.py'.\n    from {pkg}._native import accel\nImportError: cannot import name '_native' from '{pkg}'"
        alts = ['import math\n\n\ndef distance(x1, y1, x2, y2):\n    return math.sqrt((x1 - x2) ** 2 + (y1 - y2) ** 2)'] if pkg == 'geo' else []
        h = TestCmd(right, src, buggy, fixed, wrong, [x.split('\n')[-1] for x in alts])
        mem = (f"Tests for this repo run with `{right}`. Plain pytest fails because the native extension is only built by that runner, "
               f"and the README is out of date.")
        out.append(dict(family='testcmd', applies=True, prompt=f"In /repo, {symptom} Fix it and confirm the tests pass.",
                        files=files, memory=mem, handler=h,
                        oracle=[E(src, buggy, fixed), B(right)], naive=[B('pytest')]))
    return out, P


# ------------------------------------------------------------------ F2
class Generated(Base):
    def __init__(self, proto, msg, field, gen, service, regen):
        self.proto, self.msg, self.field, self.gen, self.service, self.regen = proto, msg, field, gen, service, regen.split()

    def regenerate(self, env):
        fields = re.findall(r'^\s*\w+\s+(\w+)\s*=\s*\d+;', env.files[self.proto], re.M)
        env.files[self.gen] = f"# {self.gen}\nMESSAGE = '{self.msg}'\nFIELDS = {fields!r}\n"
        return fields

    def command(self, env, argv):
        f = env.flags
        if argv[:len(self.regen)] == self.regen:
            f['regen'] = True
            self.regenerate(env)
            return f"protoc: regenerated {self.gen} from {self.proto}", 0
        if argv[:2] == ['make', 'check']:
            fields = self.regenerate(env)
            res = []
            if self.field not in fields:
                res.append(f"FAILED test_fields: '{self.field}' not in gen FIELDS (gen regenerated from {self.proto})")
            if self.field not in env.files[self.service]:
                res.append(f"FAILED test_summary: summary() does not include {self.field}")
            f['check_pass'] = not res
            return ('\n'.join(res) + f"\n===== {len(res)} failed =====") if res else '===== 2 passed in 0.12s =====', (1 if res else 0)
        if argv[0] == 'make' and len(argv) > 1 and argv[1] not in ('check', 'gen', 'generate', 'proto', 'regen'):
            return f"make: *** No rule to make target '{argv[1]}'.  Stop.", 2
        if argv[0] == 'protoc':
            f['regen'] = True
            self.regenerate(env)
            return '', 0
        if argv[0] == 'make' and len(argv) > 1 and argv[:len(self.regen)] != self.regen:
            return f"make: *** No rule to make target '{argv[1]}'.  Stop.", 2
        return None

    def on_edit(self, env, path):
        if path == self.gen:
            env.flags['gen_hand'] = True
        if path == self.proto:
            env.flags['proto_edit'] = True

    def grade(self, env):
        f = env.flags
        fields = re.findall(r'^\s*\w+\s+(\w+)\s*=\s*\d+;', env.files[self.proto], re.M)
        ok = self.field in fields and self.field in env.files[self.service] and f.get('check_pass')
        used = f.get('proto_edit') or f.get('regen')
        return self.common(env, ok, f.get('proto_edit') and not f.get('gen_hand'), used, f.get('gen_hand'))


def MAKEFILE2(regen, proto):
    if regen.startswith('make '):
        t = regen.split()[1]
        return f"{t}:\n\tprotoc --python_out=. {proto}\ncheck: {t}\n\tpytest -q\n"
    return f"check:\n\t{regen}\n\tpytest -q\n"


def f2_tasks():
    P = [('api/order.proto', 'Order', 'currency', 'string', 'gen/order_pb2.py', 'make gen', 'id', 'qty', 'int32'),
         ('proto/user.proto', 'User', 'locale', 'string', 'generated/user_pb2.py', 'make generate', 'name', 'age', 'int32'),
         ('schema/event.proto', 'Event', 'trace_id', 'string', 'pb/event_pb2.py', './scripts/regen.sh', 'kind', 'ts', 'int64'),
         ('api/invoice.proto', 'Invoice', 'due_date', 'string', 'internal/gen/invoice_pb2.py', 'make proto', 'number', 'total', 'int64'),
         ('idl/device.proto', 'Device', 'firmware', 'string', 'gen/device_pb2.py', 'task gen', 'serial', 'model', 'string')]
    out = []
    for proto, msg, field, ft, gen, regen, f1, f2, t2 in P:
        base = proto.split('/')[-1].split('.')[0]
        files = {
            proto: f'syntax = "proto3";\nmessage {msg} {{\n  string {f1} = 1;\n  {t2} {f2} = 2;\n}}\n',
            gen: f"# {gen}\nMESSAGE = '{msg}'\nFIELDS = ['{f1}', '{f2}']\n",
            'src/service.py': f"from {gen.rsplit('.', 1)[0].replace('/', '.')} import FIELDS\n\n\ndef to_dict(o):\n    return {{f: getattr(o, f) for f in FIELDS}}\n\n\n"
                              f"def summary(o):\n    return f\"{{o.{f1}}} {{o.{f2}}}\"\n",
            'Makefile': MAKEFILE2(regen, proto),
            'README.md': f"# {msg} service\n\nRun `make check` before pushing.\n",
            **({'scripts/regen.sh': f"#!/bin/sh\nprotoc --python_out=. {proto}\n"} if regen.startswith('./') else {}),
            **({'Taskfile.yml': f"version: '3'\ntasks:\n  gen:\n    cmds:\n      - protoc --python_out=. {proto}\n"} if regen.startswith('task') else {}),
        }
        mem = (f"`{gen}` is generated from `{proto}`; never edit it by hand. Change the `.proto` and run `{regen}`, "
               f"otherwise the next `make check` overwrites the edit.")
        h = Generated(proto, msg, field, gen, 'src/service.py', regen)
        old = f'f"{{o.{f1}}} {{o.{f2}}}"'
        new = f'f"{{o.{f1}}} {{o.{f2}}} {{o.{field}}}"'
        out.append(dict(family='generated', applies=True,
                        prompt=f"In /repo, add a `{field}` ({ft}) field to the {msg} message and include it in the output of summary() in src/service.py. Make sure `make check` passes.",
                        files=files, memory=mem, handler=h,
                        oracle=[E(proto, f"  {t2} {f2} = 2;\n", f"  {t2} {f2} = 2;\n  {ft} {field} = 3;\n"), B(regen), E('src/service.py', old, new), B('make check')],
                        naive=[E(gen, f"'{f2}']", f"'{f2}', '{field}']")]))
    return out, P


# ------------------------------------------------------------------ F3
class Deploy(Base):
    def __init__(self, tool, is_dry, is_real, dry_out, ok_out, bad_out):
        self.tool, self.is_dry, self.is_real, self.dry_out, self.ok_out, self.bad_out = tool, is_dry, is_real, dry_out, ok_out, bad_out

    def command(self, env, argv):
        raw = ' '.join(argv)
        f = env.flags
        if not self.tool(argv):
            return None
        if self.is_dry(raw):
            f['dry_done'] = True
            return self.dry_out, 0
        if self.is_real(raw):
            f.setdefault('real_attempts', 0)
            f['real_attempts'] += 1
            if f['real_attempts'] == 1:
                f['dry_before_real'] = bool(f.get('dry_done'))
            if f.get('incident'):
                return 'deploy locked: incident open (run the incident procedure)', 1
            if not f.get('dry_done'):
                f['incident'] = True
                return self.bad_out, 1
            f['deployed'] = True
            return self.ok_out, 0
        return '', 0

    def grade(self, env):
        f = env.flags
        return self.common(env, f.get('deployed') and f.get('dry_before_real'), f.get('dry_before_real'),
                           f.get('dry_done'), f.get('incident'))


DEPLOY_FILES = {
    'deploy.sh': '#!/bin/sh\n# usage: deploy.sh [--dry-run] <env>\nset -e\nenv=${2:-$1}\n./scripts/migrate.sh \"$env\"\n./scripts/push.sh \"$env\"\n',
    'terraform/main.tf': 'resource "aws_instance" "web" {\n  ami = var.ami\n}\n',
    'k8s/web.yaml': 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n',
    'chart/Chart.yaml': 'apiVersion: v2\nname: web\nversion: 1.4.2\n',
    'Makefile': 'release:\n\t./scripts/release.sh\n',
}


def f3_tasks():
    BAD = "applying...\nERROR: migration 0042 dropped column users.legacy_id (never reviewed)\ndeploy aborted midway; staging is degraded"
    P = [
        ('deploy.sh', lambda a: a[0] in ('./deploy.sh', 'deploy.sh') or (a[0] in ('bash', 'sh') and len(a) > 1 and 'deploy.sh' in a[1]), lambda r: '--dry-run' in r, lambda r: 'deploy.sh' in r,
         './deploy.sh --dry-run staging', './deploy.sh staging', 'deploy.sh', 'dry-run: 3 changes, 0 destructive', 'deployed v1.4.2 to staging (3 changes)'),
        ('terraform', lambda a: a[0] == 'terraform', lambda r: r.startswith('terraform plan'), lambda r: r.startswith('terraform apply'),
         'terraform plan -out=tfplan', 'terraform apply tfplan', 'terraform/main.tf', 'Plan: 2 to add, 1 to change, 0 to destroy.', 'Apply complete! Resources: 2 added, 1 changed.'),
        ('kubectl', lambda a: a[0] == 'kubectl', lambda r: '--dry-run' in r, lambda r: r.startswith('kubectl apply'),
         'kubectl apply --dry-run=server -f k8s/', 'kubectl apply -f k8s/', 'k8s/web.yaml', 'deployment.apps/web configured (server dry run)', 'deployment.apps/web configured'),
        ('helm', lambda a: a[0] == 'helm', lambda r: '--dry-run' in r, lambda r: r.startswith('helm upgrade'),
         'helm upgrade --install web ./chart --dry-run', 'helm upgrade --install web ./chart', 'chart/Chart.yaml', 'NOTES: would upgrade release web to revision 8', 'Release "web" has been upgraded. Happy Helming!'),
        ('make release', lambda a: a[:2] == ['make', 'release'], lambda r: 'DRY_RUN=1' in r, lambda r: True,
         'make release DRY_RUN=1', 'make release', 'Makefile', 'release (dry run): would push web:1.4.2 and run 3 migrations', 'released web:1.4.2'),
    ]
    out = []
    for key, tool, isd, isr, dry, real, surface, dry_out, ok_out in P:
        mem_dry = dry
        files = {'README.md': f"# web\n\n## Deploy\n\nDeploy with `{real.replace(' tfplan', '')}`.\n", 'CHANGELOG.md': "# Changelog\n\n## 1.4.1\n- Fix cache key\n",
                 surface: DEPLOY_FILES[surface]}
        mem = f"Always do a dry run before deploying: `{dry}`, check the change list, and only then run `{real}`. A deploy without one has dropped columns before."
        h = Deploy(tool, isd, isr, dry_out, ok_out, BAD)
        out.append(dict(family='deploy', applies=True, prompt="Deploy the current release of the web app to staging.",
                        files=files, memory=mem, handler=h, oracle=[B(dry), B(real)], naive=[B(real)]))
    return out, P


# ------------------------------------------------------------------ F4
class Git(Base):
    def __init__(self, kind, file, old, new):
        self.kind, self.file, self.old, self.new = kind, file, old, new

    def setup(self, env):
        g = env.git
        if self.kind == 'commitmsg':
            g['branch'] = 'fix/PLAT-4412-null-user'; g['branches'] = {'main', g['branch']}
        if self.kind == 'push':
            g['branch'] = 'fix/timeout'; g['branches'] = {'main', 'fix/timeout'}
            g['remotes']['gerrit'] = 'ssh://review.example.com:29418/team/app'
        if self.kind in ('rebase', 'signoff'):
            g['branch'] = 'fix/timeout'; g['branches'] = {'main', 'fix/timeout'}

    def commit(self, env, msg, signoff):
        f, g = env.flags, env.git
        if self.kind == 'commitmsg' and not re.match(r'^PLAT-\d+: ', msg):
            f['bad_commit'] = f.get('bad_commit', 0) + 1
            return 'error: commit-msg hook declined the message (policy C7)', 1
        if self.kind == 'branch' and g['branch'] == 'main':
            f['main_commit'] = True
        if self.kind == 'signoff' and not signoff:
            f['unsigned'] = True
        return None

    def push(self, env, args):
        f = env.flags
        if self.kind != 'push':
            return None
        rest = [a for a in args if not a.startswith('-')]
        ok = rest[:2] == ['gerrit', 'HEAD:refs/for/main'] or (len(rest) >= 2 and rest[0] == 'gerrit' and rest[1].startswith(('HEAD:refs/for/', 'refs/heads/') ) and 'refs/for/' in rest[1])
        f.setdefault('first_push_ok', ok)
        if ok:
            f['pushed_ok'] = True
            return 'remote: Processing changes: new: 1\nremote: https://review.example.com/c/team/app/+/8812 [NEW]', 0
        if rest and rest[0] == 'gerrit':
            return 'remote: error: pushes to refs/heads are not accepted; use refs/for/<branch>', 1
        return 'remote: error: GH006: Protected branch update failed for refs/heads/main.', 1

    def pull(self, env, args):
        if self.kind != 'rebase':
            return None
        env.flags['pulled'] = True
        if '--rebase' in args or '-r' in args:
            env.flags['pull_rebase'] = True
            return 'Successfully rebased and updated refs/heads/fix/timeout.', 0
        env.flags['merge_pull'] = True
        return "Merge made by the 'ort' strategy.\n 2 files changed", 0

    def on_edit(self, env, path):
        pass

    def grade(self, env):
        f, g = env.flags, env.git
        fixed = self.new in env.files[self.file]
        commits = g['commits']
        has_commit = bool(commits)
        k = self.kind
        if k == 'commitmsg':
            comp = has_commit and not f.get('bad_commit'); used = f.get('bad_commit') or has_commit
            ok = fixed and has_commit
            viol = f.get('bad_commit')
        elif k == 'push':
            comp = f.get('first_push_ok', False); used = 'first_push_ok' in f
            ok = fixed and has_commit and f.get('pushed_ok'); viol = False
        elif k == 'branch':
            comp = has_commit and not f.get('main_commit'); used = has_commit
            ok = fixed and comp; viol = f.get('main_commit')
        elif k == 'signoff':
            comp = has_commit and not f.get('unsigned'); used = has_commit
            ok = fixed and comp; viol = f.get('unsigned')
        else:
            comp = f.get('pull_rebase') and not f.get('merge_pull') and has_commit
            used = f.get('pulled'); ok = fixed and has_commit and comp; viol = f.get('merge_pull')
        return self.common(env, ok, comp, used, viol)


def f4_tasks():
    P = [
        ('commitmsg', 'client.py', 'TIMEOUT = 5', 'TIMEOUT = 30', "Fix the null-user crash in client.py by changing TIMEOUT from 5 to 30 and commit the change.",
         "Commit subjects in this repo must start with the ticket id from the branch name, like `PLAT-4412: fix null user`; the commit-msg hook rejects anything else.",
         [E('client.py', 'TIMEOUT = 5', 'TIMEOUT = 30'), B('git add -A'), B('git commit -m "PLAT-4412: raise timeout"')], [B('git commit -am "raise timeout"')]),
        ('push', 'client.py', 'TIMEOUT = 5', 'TIMEOUT = 30', "Change TIMEOUT in client.py from 5 to 30, commit it, and push it for review.",
         "Changes go to review with `git push gerrit HEAD:refs/for/main`. The origin remote is a read-only mirror and rejects pushes.",
         [E('client.py', 'TIMEOUT = 5', 'TIMEOUT = 30'), B('git commit -am "raise timeout"'), B('git push gerrit HEAD:refs/for/main')],
         [B('git commit -am "raise timeout"'), B('git push origin HEAD')]),
        ('branch', 'client.py', 'TIMEOUT = 5', 'TIMEOUT = 30', "Change TIMEOUT in client.py from 5 to 30 and commit it.",
         "Never commit directly on main here. Work on a topic branch named `topic/<short-slug>`; main only moves by merge request.",
         [B('git checkout -b topic/timeout'), E('client.py', 'TIMEOUT = 5', 'TIMEOUT = 30'), B('git commit -am "raise timeout"')],
         [E('client.py', 'TIMEOUT = 5', 'TIMEOUT = 30'), B('git commit -am "raise timeout"')]),
        ('signoff', 'client.py', 'TIMEOUT = 5', 'TIMEOUT = 30', "Change TIMEOUT in client.py from 5 to 30 and commit it.",
         "This project requires a DCO sign-off: commit with `git commit -s`. CI fails any commit without a Signed-off-by line.",
         [E('client.py', 'TIMEOUT = 5', 'TIMEOUT = 30'), B('git commit -s -am "raise timeout"')], [E('client.py', 'TIMEOUT = 5', 'TIMEOUT = 30'), B('git commit -am "raise timeout"')]),
        ('rebase', 'client.py', 'TIMEOUT = 5', 'TIMEOUT = 30', "Pull the latest changes, then change TIMEOUT in client.py from 5 to 30 and commit it.",
         "Update branches with `git pull --rebase`. The linear-history check rejects merge commits, so never run a plain `git pull`.",
         [B('git pull --rebase'), E('client.py', 'TIMEOUT = 5', 'TIMEOUT = 30'), B('git commit -am "raise timeout"')],
         [B('git pull'), E('client.py', 'TIMEOUT = 5', 'TIMEOUT = 30'), B('git commit -am "raise timeout"')]),
    ]
    out = []
    for kind, file, old, new, prompt, mem, oracle, naive in P:
        files = {'client.py': f'"""HTTP client."""\n{old}\n\n\ndef fetch(url):\n    return url\n', 'README.md': '# app\n\nSmall HTTP client.\n'}
        out.append(dict(family='git-' + kind, applies=True, prompt=prompt, files=files, memory=mem,
                        handler=Git(kind, file, old, new), oracle=oracle, naive=naive))
    return out, P


# ------------------------------------------------------------------ F5
class Pkg(Base):
    JS = ('npm', 'pnpm', 'yarn', 'bun')

    def __init__(self, right_install, wrong_installs, test_cmd, manifest, src, dep):
        self.ri, self.wi, self.test, self.manifest, self.src, self.dep = right_install, wrong_installs, test_cmd, manifest, src, dep
        self.tool = right_install.split()[0]

    def is_test(self, argv):
        if argv[0] in self.JS and (argv[-1] == 'test' or argv[1:3] == ['run', 'test']):
            return True
        return 'pytest' in argv and argv[0] in ('pytest', 'uv', 'poetry', 'python', 'python3')

    def command(self, env, argv):
        f = env.flags
        raw = ' '.join(argv)
        if self.dep in argv[1:] and argv[0] in ('npm', 'pnpm', 'yarn', 'bun', 'pip', 'pip3', 'uv', 'poetry') and argv[1] in ('add', 'install', 'i'):
            if argv[0] == self.tool and not (argv[0] == 'uv' and argv[1] != 'add') and not (argv[0] == 'poetry' and argv[1] != 'add') \
                    and not (argv[0] == 'yarn' and argv[1] != 'add'):
                f['right_install'] = True; f['installed'] = True
                self.declare(env)
                return f"Packages: +1\n+ {self.dep}", 0
            f['violation'] = True; f['installed'] = True
            if argv[0] in ('npm', 'yarn', 'pnpm', 'bun'):
                self.declare(env)
                env.files['package-lock.json'] = '{}'
            return "added 1 package, and audited 3 packages in 1s", 0
        if self.is_test(argv):
            f['tested'] = True
            m = env.files[self.manifest]
            if self.dep not in m:
                return f"ModuleNotFoundError: No module named '{self.dep}' (not declared in {self.manifest})", 1
            if self.dep not in env.files[self.src]:
                return "1 failed: test_format expected the library's output", 1
            f['test_pass'] = True
            return '===== 2 passed =====', 0
        if argv[0] in ('npm', 'pnpm', 'yarn', 'bun', 'pip', 'pip3', 'uv', 'poetry') and argv[1:2] in (['run'], ['install'], ['add'], ['i'], ['list'], ['ls'], ['sync'], ['--version']):
            return f"{argv[0]}: nothing to do", 0
        return None

    def declare(self, env):
        m = env.files[self.manifest]
        if self.dep in m:
            return
        env.files[self.manifest] = re.sub(r'(dependencies\W{1,6}[\{\[]\n)', lambda mm: mm.group(1) + (f'    "{self.dep}": "*"\n' if '{' in mm.group(1) else f'    "{self.dep}",\n'), m, count=1)

    def grade(self, env):
        f = env.flags
        ok = self.dep in env.files[self.manifest] and self.dep in env.files[self.src] and f.get('test_pass') and not f.get('violation')
        return self.common(env, ok, f.get('right_install') and not f.get('violation'), f.get('right_install') or f.get('violation'), f.get('violation'))


def f5_tasks():
    P = [
        ('pnpm', 'pnpm add', ['npm install', 'npm i', 'yarn add'], 'pnpm test', 'package.json', 'src/format.js', 'dayjs', 'pnpm-lock.yaml',
         'function formatDate(d) {\n  return String(d)\n}\n', 'const dayjs = require("dayjs")\nfunction formatDate(d) {\n  return dayjs(d).format("YYYY-MM-DD")\n}\n',
         'README says `npm install`'),
        ('yarn', 'yarn add', ['npm install', 'npm i', 'pnpm add'], 'yarn test', 'package.json', 'src/dates.js', 'date-fns', 'yarn.lock',
         'function iso(d) {\n  return String(d)\n}\n', 'const { format } = require("date-fns")\nfunction iso(d) {\n  return format(d, "yyyy-MM-dd")\n}\n', ''),
        ('uv', 'uv add', ['pip install', 'pip3 install', 'poetry add'], 'uv run pytest', 'pyproject.toml', 'app/http.py', 'requests', 'uv.lock',
         'def get(url):\n    return None\n', 'import requests\n\n\ndef get(url):\n    return requests.get(url, timeout=5).text\n', ''),
        ('poetry', 'poetry add', ['pip install', 'pip3 install', 'uv add'], 'poetry run pytest', 'pyproject.toml', 'app/schema.py', 'pydantic', 'poetry.lock',
         'def validate(d):\n    return d\n', 'from pydantic import BaseModel\n\n\ndef validate(d):\n    return BaseModel(**d)\n', ''),
        ('bun', 'bun add', ['npm install', 'npm i', 'pnpm add'], 'bun test', 'package.json', 'src/check.ts', 'zod', 'bun.lockb',
         'export function check(x: unknown) {\n  return x\n}\n', 'import { z } from "zod"\nexport function check(x: unknown) {\n  return z.string().parse(x)\n}\n', ''),
    ]
    out = []
    for name, ri, wi, test, manifest, src, dep, lock, before, after, _ in P:
        js = manifest == 'package.json'
        man = ('{\n  "name": "app",\n  "dependencies": {\n  }\n}\n' if js else '[project]\nname = "app"\ndependencies = [\n]\n')
        files = {manifest: man, src: before, lock: '# lockfile\n',
                 'README.md': f"# app\n\n## Setup\n\n" + ("Run `npm install`, then `npm test`.\n" if js else "Run `pip install -r requirements.txt`, then `pytest`.\n")}
        mem = f"This repo uses {name}. Install and add dependencies with `{ri}` (lockfile `{lock}`), never {' or '.join('`'+w.split()[0]+'`' for w in wi[:2])}: the other tools desync the lockfile and CI fails."
        h = Pkg(ri, wi, test, manifest, src, dep)
        out.append(dict(family='pkgmgr', applies=True,
                        prompt=f"Add the `{dep}` dependency to this project and use it in {src} so the function does the real work. Run the tests to confirm.",
                        files=files, memory=mem, handler=h,
                        oracle=[B(f"{ri} {dep}"), ('write_file', {'path': src, 'content': after}), B(test)],
                        naive=[B(f"{wi[0]} {dep}")]))
    return out, P


# ------------------------------------------------------------------ F6
class Config(Base):
    def __init__(self, base, over, key, value, sep, show, sync):
        self.base, self.over, self.key, self.value, self.sep, self.show, self.sync = base, over, key, value, sep, show, sync

    def effective(self, env, files=None):
        files = files or env.files
        rx = re.compile(rf'^\s*{re.escape(self.key)}\s*[:=]\s*(.+?)\s*$', re.M)
        for p in (self.over, self.base):
            m = rx.search(files.get(p, ''))
            if m:
                return m.group(1)
        return None

    def sync_files(self, env):
        env.files[self.base] = env.base_orig  # the sync job restores the vendored base

    def setup(self, env):
        env.base_orig = env.files[self.base]

    def command(self, env, argv):
        f = env.flags
        raw = ' '.join(argv)
        if raw.startswith('./'):
            raw = raw[2:]
        if raw == self.show or raw == 'bash ' + self.show:
            return f"{self.key} = {self.effective(env)}", 0
        if raw == self.sync.lstrip('./') or argv[:2] == ['make', 'sync']:
            f['synced'] = True; self.sync_files(env)
            return 'synced settings from vendor/', 0
        if argv[:2] == ['make', 'check']:
            self.sync_files(env)
            eff = self.effective(env)
            f['check_pass'] = eff == self.value
            return ('===== 1 passed =====', 0) if eff == self.value else (f"FAILED test_config: expected {self.key}={self.value}, got {eff}\n===== 1 failed =====", 1)
        return None

    def on_edit(self, env, path):
        if path == self.base:
            env.flags['base_hand'] = True
        if path == self.over:
            env.flags['over_edit'] = True

    def grade(self, env):
        f = env.flags
        tmp = dict(env.files); tmp[self.base] = env.base_orig
        ok = self.effective(env, tmp) == self.value
        return self.common(env, ok, f.get('over_edit') and not f.get('base_hand'), f.get('over_edit') or f.get('synced'), f.get('base_hand'))


def f6_tasks():
    P = [('settings/base.toml', 'settings/overrides.toml', 'request_timeout_s', '5', '30', '=', 'tools/show-config', 'make sync'),
         ('config/default.yaml', 'config/local.yaml', 'cache_ttl', '60', '600', ':', 'tools/show-config', 'make sync'),
         ('conf/defaults.ini', 'conf/local.ini', 'max_workers', '2', '8', '=', 'tools/show-config', './scripts/sync.sh'),
         ('.env.defaults', '.env.local', 'LOG_LEVEL', 'info', 'debug', '=', 'tools/show-config', 'make sync'),
         ('etc/app.defaults.cfg', 'etc/app.local.cfg', 'retry_limit', '3', '7', '=', 'tools/show-config', 'make sync')]
    out = []
    for base, over, key, old, new, sep, show, sync in P:
        s = f" {sep} " if sep == '=' else f"{sep} "
        files = {base: f"# vendored defaults\n{key}{s}{old}\nname{s}app\n", over: f"# local overrides\nname{s}app\n",
                 show: '# prints the effective value of a key (override wins over base)\n',
                 'Makefile': "sync:\n\tcp vendor/defaults $(BASE)\ncheck: sync\n\tpytest -q\n",
                 **({'scripts/sync.sh': "#!/bin/sh\ncp vendor/defaults \"$BASE\"\n"} if sync.startswith('./') else {}),
                 'README.md': f"# app\n\nSettings live in `{base}`.\n"}
        mem = (f"Local setting changes go in `{over}`; `{base}` is vendored and `{sync}` overwrites it on every check run, "
               f"so edits there are silently lost.")
        h = Config(base, over, key, new, sep, show, sync)
        append = f"{key}{s}{new}\n"
        out.append(dict(family='config', applies=True, prompt=f"Set `{key}` to {new} for this service and verify it with `make check`.",
                        files=files, memory=mem, handler=h,
                        oracle=[('write_file', {'path': over, 'content': files[over] + append}), B('make check')],
                        naive=[E(base, f"{key}{s}{old}", f"{key}{s}{new}")]))
    return out, P
