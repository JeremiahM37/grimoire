"""Agent under test: local qwen3.6:35b-a3b through Ollama with native tool calling.

`think` is left at the model default (on): turning it off silently disables
tool calls for this model on the Ollama builds in use (see the homelab notes).
No Claude calls anywhere in this harness.
"""
import json
import time
import urllib.request

OLLAMA = 'http://100.127.85.58:11434'
MODEL = 'qwen3.6:35b-a3b'
NUM_CTX = 16384
MAX_STEPS = 12

TOOLS = [
    {'type': 'function', 'function': {'name': 'bash', 'description': 'Run a shell command in the repo (cwd /repo) and return its output.',
        'parameters': {'type': 'object', 'properties': {'command': {'type': 'string'}}, 'required': ['command']}}},
    {'type': 'function', 'function': {'name': 'read_file', 'description': 'Read a file.',
        'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}}, 'required': ['path']}}},
    {'type': 'function', 'function': {'name': 'list_files', 'description': 'List a directory.',
        'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}}, 'required': ['path']}}},
    {'type': 'function', 'function': {'name': 'edit_file', 'description': 'Replace one exact occurrence of old_string with new_string in a file.',
        'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}, 'old_string': {'type': 'string'}, 'new_string': {'type': 'string'}},
                       'required': ['path', 'old_string', 'new_string']}}},
    {'type': 'function', 'function': {'name': 'write_file', 'description': 'Create or overwrite a file with the given content.',
        'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}, 'content': {'type': 'string'}}, 'required': ['path', 'content']}}},
]
CLAUDE_NAME = {'bash': 'Bash', 'read_file': 'Read', 'list_files': 'Glob', 'edit_file': 'Edit', 'write_file': 'Write'}

SYSTEM = ("You are a coding agent working in a git repository mounted at /repo. Use the tools to inspect and change files "
          "and to run commands. Work efficiently and stop once the task is done, then reply with a short summary of what you did.")


def system_prompt(memories):
    if not memories:
        return SYSTEM
    lines = ["", "Grimoire memory: notes the user saved earlier. Apply any that fit the task. "
             "When an item changes what you do, cite its tag once, like (m:3e99)."]
    lines += [f"- (m:{m['tag']}) {m['text']}" for m in memories]
    return SYSTEM + '\n' + '\n'.join(lines)


def chat(messages, seed, num_predict=3000, temperature=0.6, retries=3):
    body = {'model': MODEL, 'messages': messages, 'tools': TOOLS, 'stream': False,
            'options': {'num_ctx': NUM_CTX, 'temperature': temperature, 'seed': seed, 'num_predict': num_predict}}
    last = None
    for a in range(retries):
        try:
            req = urllib.request.Request(OLLAMA + '/api/chat', json.dumps(body).encode(), {'Content-Type': 'application/json'})
            with urllib.request.urlopen(req, timeout=600) as r:
                return json.load(r)['message']
        except Exception as e:  # network blips on a long run
            last = e
            time.sleep(3 * (a + 1))
    raise RuntimeError(f'ollama failed: {last}')


def clean_calls(msg):
    out = []
    for tc in msg.get('tool_calls') or []:
        fn = tc.get('function', {})
        args = fn.get('arguments') or {}
        if isinstance(args, str):
            try:
                args = json.loads(args)
            except Exception:
                args = {}
        out.append(dict(name=fn.get('name', ''), args=args))
    return out


def sig(name, args):
    """Normalised action signature used to compare an action across runs."""
    if name == 'bash':
        cmd = str(args.get('command', '')).strip()
        first = cmd.split('&&')[0].split(';')[0].strip().split()
        prog = [t for t in first if '=' not in t.split('/')[-1] or t.startswith('-')]
        flags = sorted(t for t in first if t.startswith('-'))
        words = [t for t in first if not t.startswith('-')][:3]
        extra = ''
        if len(first) > 1 and first[0] == 'git' and first[1] == 'commit':
            extra = ' msg=' + cmd.split('-m', 1)[1].strip().strip('"\'')[:10] if '-m' in cmd else ''
        return 'bash:' + ' '.join(words) + ' ' + ' '.join(flags) + extra
    if name in ('edit_file', 'write_file', 'read_file', 'list_files'):
        return f"{name}:{args.get('path', '')}"
    return name


def run_agent(task, env, memories, seed, log=None):
    """Run one episode. Returns dict(steps, final, messages)."""
    messages = [{'role': 'system', 'content': system_prompt(memories)}, {'role': 'user', 'content': task['prompt']}]
    steps = []
    final = ''
    for i in range(MAX_STEPS):
        msg = chat(messages, seed + i)
        calls = clean_calls(msg)
        step = dict(content=msg.get('content') or '', thinking=msg.get('thinking') or '', calls=[], history_len=len(messages))
        am = {'role': 'assistant', 'content': msg.get('content') or ''}
        if calls:
            am['tool_calls'] = [{'function': {'name': c['name'], 'arguments': c['args']}} for c in calls]
        messages.append(am)
        if not calls:
            final = msg.get('content') or ''
            steps.append(step)
            break
        for c in calls:
            out, rc = env.call(c['name'], c['args'])
            step['calls'].append(dict(name=c['name'], args=c['args'], out=out, err=rc != 0))
            messages.append({'role': 'tool', 'tool_name': c['name'], 'content': out[:3000] or '(no output)'})
        steps.append(step)
    return dict(steps=steps, final=final, messages=messages)
