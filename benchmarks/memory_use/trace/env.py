"""Simulated coding environment: a fake repo, a small bash interpreter, a fake git.

Everything is deterministic and in-memory. Task families (families.py) plug in
through a handler object with `command(env, argv)` (first look at every simple
command; return (output, rc) or None to fall through to the builtins) and
`grade(env)`.
"""
import fnmatch
import re
import shlex


class Env:
    def __init__(self, files, handler, branch='main'):
        self.files = dict(files)
        self.base = dict(files)  # as committed
        self.h = handler
        self.log = []          # every tool call: dict(tool, args, out, err)
        self.hand_edits = []   # (path, how) written by the agent, not by make
        self.cmds = []         # every simple command executed (argv lists), in order
        self.git = dict(branch=branch, branches={branch}, commits=[], staged=set(),
                        pushes=[], pulls=[], remotes={'origin': 'git@git.example.com:team/app.git'})
        self.flags = {}        # family scratch: violations, state
        self.cwd = '/repo'
        handler.setup(self)

    # ----- tools -------------------------------------------------------
    def call(self, name, args):
        try:
            if name == 'bash':
                out, rc = self.bash(str(args.get('command', '')))
            elif name == 'read_file':
                p = self.norm(args.get('path', ''))
                if p in self.files:
                    out, rc = self.files[p], 0
                else:
                    out, rc = f"read_file: {p}: no such file", 1
            elif name == 'list_files':
                out, rc = self.ls(self.norm(args.get('path', '.')))
            elif name == 'write_file':
                p = self.norm(args.get('path', ''))
                self.files[p] = str(args.get('content', ''))
                self.hand_edits.append((p, 'write'))
                self.h.on_edit(self, p)
                out, rc = f"wrote {p} ({len(self.files[p])} bytes)", 0
            elif name == 'edit_file':
                p = self.norm(args.get('path', ''))
                old, new = str(args.get('old_string', '')), str(args.get('new_string', ''))
                if p not in self.files:
                    out, rc = f"edit_file: {p}: no such file", 1
                elif not old or old not in self.files[p]:
                    out, rc = "edit_file: old_string not found in file", 1
                elif self.files[p].count(old) > 1:
                    out, rc = "edit_file: old_string matches more than once; add context", 1
                else:
                    self.files[p] = self.files[p].replace(old, new)
                    self.hand_edits.append((p, 'edit'))
                    self.h.on_edit(self, p)
                    out, rc = f"edited {p}", 0
            else:
                out, rc = f"unknown tool {name}", 1
        except Exception as e:  # a malformed call must not kill the run
            out, rc = f"tool error: {e}", 1
        self.log.append(dict(tool=name, args=args, out=out, err=rc != 0))
        return out, rc

    def norm(self, p):
        p = str(p).strip()
        if p.startswith('/repo/'):
            p = p[6:]
        if p.rstrip('/') == '/repo':
            return '.'
        p = re.sub(r'^\./+', '', p)
        return p.rstrip('/') if p not in ('', '.') else p

    def ls(self, d):
        d = '' if d in ('.', '') else d.rstrip('/')
        names = set()
        for f in self.files:
            if d == '' or f.startswith(d + '/'):
                rest = f[len(d) + 1:] if d else f
                names.add(rest.split('/')[0] + ('/' if '/' in rest else ''))
        if not names and d not in ('', ) and d not in self.files:
            return f"ls: cannot access '{d}': No such file or directory", 2
        if d in self.files:
            return d, 0
        return '\n'.join(sorted(names)), 0

    # ----- bash --------------------------------------------------------
    def bash(self, cmd):
        lex = shlex.shlex(cmd, posix=True, punctuation_chars=True)
        lex.whitespace_split = True
        try:
            toks = list(lex)
        except ValueError as e:
            return f"bash: syntax error: {e}", 2
        segs, cur, ops = [], [], []
        for t in toks:
            if t in ('&&', ';', '||', '&'):
                segs.append(cur); ops.append(t); cur = []
            else:
                cur.append(t)
        segs.append(cur)
        outs, rc = [], 0
        for i, seg in enumerate(segs):
            if not seg:
                continue
            if i > 0 and ops[i - 1] == '&&' and rc != 0:
                continue
            if i > 0 and ops[i - 1] == '||' and rc == 0:
                continue
            o, rc = self.simple(seg)
            if o:
                outs.append(o)
        return '\n'.join(outs), rc

    def simple(self, argv):
        # drop pipes (keep the producer), keep redirect info
        redirect = None
        if '|' in argv:
            argv = argv[:argv.index('|')]
        clean = []
        i = 0
        while i < len(argv):
            t = argv[i]
            if t in ('>', '>>'):
                redirect = (t, argv[i + 1] if i + 1 < len(argv) else '')
                i += 2
                continue
            if re.fullmatch(r'\d?>&?\d?|2>|2>&1|>&|>/dev/null', t) or t in ('2', '>&', '1'):
                i += 1
                if t in ('2',) and i < len(argv) and argv[i].startswith('>'):
                    i += 1
                continue
            clean.append(t)
            i += 1
        argv = clean
        if not argv:
            return '', 0
        # leading VAR=value
        while argv and re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*=.*', argv[0]) and len(argv) > 1:
            argv = argv[1:]
        self.cmds.append(list(argv))
        r = self.h.command(self, argv)
        if r is None:
            r = self.builtin(argv, redirect)
        return r

    def builtin(self, argv, redirect=None):
        p, a = argv[0], argv[1:]
        if p == 'cd':
            return '', 0
        if p == 'pwd':
            return self.cwd, 0
        if p in ('true', 'clear', 'set', 'export', 'source', 'mkdir', 'chmod'):
            return '', 0
        if p == 'echo':
            text = ' '.join(a)
            if redirect:
                path = self.norm(redirect[1])
                self.files[path] = (self.files.get(path, '') if redirect[0] == '>>' else '') + text + '\n'
                self.hand_edits.append((path, 'redirect'))
                self.h.on_edit(self, path)
                return '', 0
            return text, 0
        if p == 'ls':
            paths = [x for x in a if not x.startswith('-')]
            return self.ls(self.norm(paths[0]) if paths else '.')
        if p in ('cat', 'head', 'tail'):
            n, paths, i = 10, [], 0
            while i < len(a):
                if a[i] == '-n' and i + 1 < len(a):
                    n = int(a[i + 1]); i += 2; continue
                if re.fullmatch(r'-\d+', a[i]):
                    n = int(a[i][1:]); i += 1; continue
                if not a[i].startswith('-'):
                    paths.append(self.norm(a[i]))
                i += 1
            out = []
            for f in paths:
                if f not in self.files:
                    return f"{p}: {f}: No such file or directory", 1
                t = self.files[f].rstrip('\n').split('\n')
                if p == 'head':
                    t = t[:n]
                elif p == 'tail':
                    t = t[-n:]
                out.append('\n'.join(t))
            if p == 'cat' and redirect:
                return 'cat: heredoc/redirect not supported; use write_file', 1
            return '\n'.join(out), 0
        if p == 'grep':
            flags = [x for x in a if x.startswith('-')]
            rest = [x for x in a if not x.startswith('-')]
            if not rest:
                return 'usage: grep PATTERN [PATH...]', 2
            pat = rest[0]
            paths = [self.norm(x) for x in rest[1:]] or ['.']
            rx = re.compile(pat, re.I if any('i' in f for f in flags) else 0)
            hits = []
            for f in sorted(self.files):
                if not any(x in ('.', '') or f == x or f.startswith(x + '/') for x in paths):
                    continue
                for n, line in enumerate(self.files[f].split('\n'), 1):
                    if rx.search(line):
                        hits.append(f"{f}:{n}:{line}")
            if any('l' in f and not f.startswith('--') for f in flags):
                hits = sorted({h.split(':')[0] for h in hits})
            return '\n'.join(hits), (0 if hits else 1)
        if p == 'find':
            root = self.norm(a[0]) if a and not a[0].startswith('-') else '.'
            names = [a[i + 1] for i, t in enumerate(a) if t in ('-name', '-iname') and i + 1 < len(a)]
            only_dirs = '-type' in a and a[a.index('-type') + 1] == 'd'
            res = []
            if not only_dirs:
                for f in sorted(self.files):
                    if root not in ('.', '') and not (f == root or f.startswith(root + '/')):
                        continue
                    if names and not any(fnmatch.fnmatch(f.split('/')[-1], n) for n in names):
                        continue
                    res.append('./' + f)
            return '\n'.join(res), 0
        if p == 'wc':
            f = self.norm([x for x in a if not x.startswith('-')][0]) if [x for x in a if not x.startswith('-')] else ''
            return (str(self.files.get(f, '').count('\n')) + ' ' + f, 0) if f in self.files else (f"wc: {f}: No such file", 1)
        if p == 'sed' and '-n' in a:
            m = re.fullmatch(r"(\d+),(\d+)p", a[a.index('-n') + 1])
            f = self.norm(a[-1])
            if m and f in self.files:
                ls = self.files[f].split('\n')
                return '\n'.join(ls[int(m.group(1)) - 1:int(m.group(2))]), 0
        if p == 'git':
            return self.git_cmd(a)
        if p == 'which':
            known = {'ls', 'cat', 'grep', 'git', 'make', 'just', 'tox', 'nox', 'protoc', 'terraform', 'kubectl', 'helm',
                     'npm', 'pnpm', 'yarn', 'bun', 'uv', 'poetry', 'pytest', 'sh', 'bash', 'find', 'sed'}
            return ('/usr/bin/' + a[0], 0) if a and a[0] in known else ('', 1)
        if p == 'sed' and a and a[0].startswith('-i') and len(a) >= 3:
            m = re.fullmatch(r"s(.)(.*?)(?<!\\)\1(.*?)(?<!\\)\1([gI]*)", a[1])
            f = self.norm(a[-1])
            if not m or f not in self.files:
                return f"sed: cannot edit {a[-1]}", 1
            old, new = m.group(2), m.group(3)
            try:
                rx = re.compile(old)
            except re.error:
                rx = re.compile(re.escape(old))
            self.files[f] = rx.sub(new, self.files[f], count=0 if 'g' in m.group(4) else 1)
            self.hand_edits.append((f, 'sed'))
            self.h.on_edit(self, f)
            return '', 0
        if p == 'rm':
            for x in a:
                if not x.startswith('-'):
                    self.files.pop(self.norm(x), None)
            return '', 0
        if p in ('python', 'python3', 'pip', 'pip3'):
            return f"bash: {p}: command not found", 127
        return f"bash: {p}: command not found", 127

    # ----- git ---------------------------------------------------------
    def changed(self):
        ch = set()
        for f in set(self.files) | set(self.base):
            if self.files.get(f) != self.base.get(f):
                ch.add(f)
        return ch

    def git_cmd(self, a):
        g = self.git
        if not a:
            return 'usage: git <command>', 1
        sub, rest = a[0], a[1:]
        if sub == 'status':
            ch = sorted(self.changed())
            lines = [f"On branch {g['branch']}"]
            st = [f for f in ch if f in g['staged']]
            un = [f for f in ch if f not in g['staged']]
            if st:
                lines += ['Changes to be committed:'] + [f"\tmodified:   {f}" for f in st]
            if un:
                lines += ['Changes not staged for commit:'] + [f"\tmodified:   {f}" for f in un]
            if not ch:
                lines.append('nothing to commit, working tree clean')
            return '\n'.join(lines), 0
        if sub == 'diff':
            return '\n'.join(f"diff --git a/{f} b/{f}" for f in sorted(self.changed())), 0
        if sub == 'add':
            if any(x in ('.', '-A', '--all', '-u') for x in rest):
                g['staged'] |= self.changed()
            else:
                g['staged'] |= {self.norm(x) for x in rest}
            return '', 0
        if sub == 'branch':
            names = [x for x in rest if not x.startswith('-')]
            if names:
                g['branches'].add(names[0]); return '', 0
            return '\n'.join(('* ' if b == g['branch'] else '  ') + b for b in sorted(g['branches'])), 0
        if sub in ('checkout', 'switch'):
            if '-b' in rest or '-c' in rest or '-B' in rest:
                name = [x for x in rest if not x.startswith('-')][0]
                g['branches'].add(name); g['branch'] = name
                return f"Switched to a new branch '{name}'", 0
            name = [x for x in rest if not x.startswith('-')]
            if name and name[0] in g['branches']:
                g['branch'] = name[0]; return f"Switched to branch '{name[0]}'", 0
            return f"error: pathspec '{name[0] if name else ''}' did not match any file(s) known to git", 1
        if sub == 'remote':
            return '\n'.join(f"{k}\t{v} (fetch)\n{k}\t{v} (push)" for k, v in g['remotes'].items()), 0
        if sub == 'fetch':
            return '', 0
        if sub == 'log':
            if not g['commits']:
                return 'abc1234 initial import', 0
            return '\n'.join(f"{c['id']} {c['msg'].splitlines()[0]}" for c in reversed(g['commits'])) + '\nabc1234 initial import', 0
        if sub == 'commit':
            msg, signoff, allf = None, False, False
            i = 0
            while i < len(rest):
                t = rest[i]
                if t in ('-m', '--message') and i + 1 < len(rest):
                    msg = rest[i + 1]; i += 2; continue
                if t.startswith('-') and not t.startswith('--') and 'a' in t[1:] and 'm' not in t:
                    allf = True
                if t.startswith('-') and not t.startswith('--') and 'm' in t and i + 1 < len(rest):
                    msg = rest[i + 1]; allf = allf or 'a' in t; signoff = signoff or 's' in t; i += 2; continue
                if t in ('-s', '--signoff') or (t.startswith('-') and not t.startswith('--') and 's' in t):
                    signoff = True
                if t in ('-a', '--all'):
                    allf = True
                i += 1
            if allf:
                g['staged'] |= self.changed()
            if msg is None:
                return 'Aborting commit due to empty commit message.', 1
            if not g['staged']:
                return 'nothing added to commit but untracked files present / no changes added to commit', 1
            r = self.h.commit(self, msg, signoff)
            if r is not None:
                return r
            return self.do_commit(msg, signoff)
        if sub == 'push':
            r = self.h.push(self, rest)
            if r is not None:
                return r
            g['pushes'].append(rest)
            return f"To {g['remotes']['origin']}\n   abc1234..def5678  {g['branch']} -> {g['branch']}", 0
        if sub == 'pull':
            r = self.h.pull(self, rest)
            if r is not None:
                return r
            g['pulls'].append(rest)
            return 'Already up to date.', 0
        return f"git: '{sub}' is not supported in this sandbox", 1

    def do_commit(self, msg, signoff):
        g = self.git
        c = dict(id=f"{len(g['commits']) + 1:07x}", msg=msg, signoff=signoff, branch=g['branch'],
                 files=sorted(g['staged']))
        g['commits'].append(c)
        for f in g['staged']:
            if f in self.files:
                self.base[f] = self.files[f]
            else:
                self.base.pop(f, None)
        g['staged'] = set()
        return f"[{g['branch']} {c['id']}] {msg.splitlines()[0]}\n {len(c['files'])} file(s) changed", 0


class Handler:
    """Base family handler: no special behaviour."""
    def setup(self, env): pass
    def command(self, env, argv): return None
    def on_edit(self, env, path): pass
    def commit(self, env, msg, signoff): return None
    def push(self, env, args): return None
    def pull(self, env, args): return None
    def grade(self, env): raise NotImplementedError
