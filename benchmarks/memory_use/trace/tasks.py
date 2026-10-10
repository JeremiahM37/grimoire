"""The 40-task set: 30 treatment tasks (the memory changes the right action) and
10 controls (the memory is present but the task does not touch it)."""
import copy
import hashlib

import families as F
from env import Env

DISTRACTORS = [
    "The staging database is reset every night at 03:00 UTC; do not keep long-lived test data there.",
    "API docs are built with `mkdocs` and served on port 8001 during development.",
    "Prefer dataclasses over plain dicts for new domain models.",
    "Application logs go to /var/log/app/ and are rotated weekly by logrotate.",
    "The on-call handbook lives in the wiki page OPS-12.",
    "New code targets Python 3.12; 3.10 is only needed for the legacy worker.",
    "Use UTC timestamps everywhere and convert to local time only in the display layer.",
    "The metrics dashboard is the Grafana board called `app-overview`.",
    "Feature flags are managed in flags.yaml and flipped through the admin console, not by editing prod config.",
    "Integration tests that touch S3 use the localstack container on port 4566.",
]


def tag(text):
    return hashlib.sha1(text.encode()).hexdigest()[:4]


def _ctl(base, tid, prompt, check, oracle, extra_files=None):
    t = dict(base)
    t['files'] = {**base['files'], **(extra_files or {})}
    h = copy.copy(base['handler'])
    h.control = dict(check=check)
    t.update(id=tid, applies=False, prompt=prompt, handler=h, oracle=oracle, naive=[], family=base['family'])
    return t


def build():
    f1, P1 = F.f1_tasks()
    f2, P2 = F.f2_tasks()
    f3, P3 = F.f3_tasks()
    f4, P4 = F.f4_tasks()
    f5, P5 = F.f5_tasks()
    f6, P6 = F.f6_tasks()
    tasks = []
    for fam in (f1, f2, f3, f4, f5, f6):
        for i, t in enumerate(fam):
            t['id'] = f"{t['family']}-{i + 1}"
            tasks.append(t)
    W = lambda p, c: ('write_file', {'path': p, 'content': c})
    ctl = [
        _ctl(f1[0], 'ctl-testcmd-1', "Add a CONTRIBUTING.md at the repo root with a two-line project overview. Do not change any code.",
             lambda e: 'CONTRIBUTING.md' in e.files, [W('CONTRIBUTING.md', '# Contributing\nSmall pricing library.\n')]),
        _ctl(f1[2], 'ctl-testcmd-2', "Append the line `Maintainers: platform team` to the end of README.md.",
             lambda e: 'Maintainers: platform team' in e.files['README.md'], [('edit_file', {'path': 'README.md', 'old_string': '`pytest`.', 'new_string': '`pytest`.\nMaintainers: platform team'})]),
        _ctl(f2[0], 'ctl-generated-1', "In src/service.py rename the parameter `o` of to_dict to `obj`.",
             lambda e: 'def to_dict(obj)' in e.files['src/service.py'] and 'getattr(obj' in e.files['src/service.py'],
             [('edit_file', {'path': 'src/service.py', 'old_string': 'def to_dict(o):\n    return {f: getattr(o, f)', 'new_string': 'def to_dict(obj):\n    return {f: getattr(obj, f)'})]),
        _ctl(f3[0], 'ctl-deploy-1', "Add a CHANGELOG.md entry for 1.4.2 saying `Fix retry backoff`. Do not deploy anything.",
             lambda e: '1.4.2' in e.files['CHANGELOG.md'] and 'Fix retry backoff' in e.files['CHANGELOG.md'] and not e.flags.get('real_attempts'),
             [('edit_file', {'path': 'CHANGELOG.md', 'old_string': '## 1.4.1', 'new_string': '## 1.4.2\n- Fix retry backoff\n\n## 1.4.1'})]),
        _ctl(f3[2], 'ctl-deploy-2', "Add a short `Rollback` section to README.md saying to redeploy the previous tag. Do not run any deploy commands.",
             lambda e: 'Rollback' in e.files['README.md'] and not e.flags.get('real_attempts'),
             [('edit_file', {'path': 'README.md', 'old_string': '## Deploy', 'new_string': '## Rollback\n\nRedeploy the previous tag.\n\n## Deploy'})]),
        _ctl(f4[0], 'ctl-git-1', "Add a one-line docstring to fetch() in client.py. Do not commit; I will review first.",
             lambda e: '"""' in e.files['client.py'].split('def fetch')[1] and not e.git['commits'],
             [('edit_file', {'path': 'client.py', 'old_string': 'def fetch(url):\n', 'new_string': 'def fetch(url):\n    """Return the url."""\n'})]),
        _ctl(f4[3], 'ctl-git-2', "Add a one-line docstring to fetch() in client.py. Do not commit; I will review first.",
             lambda e: '"""' in e.files['client.py'].split('def fetch')[1] and not e.git['commits'],
             [('edit_file', {'path': 'client.py', 'old_string': 'def fetch(url):\n', 'new_string': 'def fetch(url):\n    """Return the url."""\n'})]),
        _ctl(f5[0], 'ctl-pkgmgr-1', "Append the line `License: MIT` to README.md.",
             lambda e: 'License: MIT' in e.files['README.md'], [('edit_file', {'path': 'README.md', 'old_string': '## Setup', 'new_string': 'License: MIT\n\n## Setup'})]),
        _ctl(f5[2], 'ctl-pkgmgr-2', "Add a one-line docstring to get() in app/http.py.",
             lambda e: '"""' in e.files['app/http.py'], [('edit_file', {'path': 'app/http.py', 'old_string': 'def get(url):\n', 'new_string': 'def get(url):\n    """Fetch a url."""\n'})]),
        _ctl(f6[0], 'ctl-config-1', "List the setting names defined in the project's default config file into docs/keys.md, one per line.",
             lambda e: 'docs/keys.md' in e.files and 'request_timeout_s' in e.files['docs/keys.md'], [W('docs/keys.md', 'request_timeout_s\nname\n')]),
    ]
    tasks += ctl
    for i, t in enumerate(tasks):
        t['relevant_tag'] = tag(t['memory'])
        ds = [DISTRACTORS[(i + k * 3) % len(DISTRACTORS)] for k in range(2)]
        t['distractors'] = ds
        t['memories'] = [dict(id='rel', text=t['memory'], tag=tag(t['memory']), relevant=True)] + \
            [dict(id=f'd{k}', text=d, tag=tag(d), relevant=False) for k, d in enumerate(ds)]
    return tasks


def make_env(task):
    h = copy.copy(task['handler'])
    return Env(task['files'], h)


def replay_calls(task, calls):
    e = make_env(task)
    for name, args in calls:
        e.call(name, args)
    return e.h.grade(e), e


if __name__ == '__main__':
    ts = build()
    bad = 0
    for t in ts:
        g, _ = replay_calls(t, t['oracle'])
        n, _ = replay_calls(t, t['naive']) if t['naive'] else ({'success': None, 'compliant': None}, None)
        ok = g['success'] and (g['compliant'] or not t['applies'])
        flag = '' if ok else '  <-- ORACLE FAILS'
        if not ok:
            bad += 1
        print(f"{t['id']:16} oracle succ={g['success']} comp={g['compliant']} viol={g['violation']} | naive succ={n['success']} comp={n['compliant']}{flag}")
    print(len(ts), 'tasks;', bad, 'bad oracles')
