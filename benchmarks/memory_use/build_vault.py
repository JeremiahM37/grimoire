"""Scratch vault for replay: the live notes vault, the agent-memory store as real
files, and instruction-file sections as notes. Lives on /mnt/bulk; never synced."""
import json
import os
import shutil

V = '/mnt/bulk/memory-use-research/vault'
shutil.rmtree(V, ignore_errors=True)
def skip(d, names):
    return [n for n in names if n in ('.grimoire', '.obsidian', '.git', '.stfolder', '.trash', 'Agent Memory', '.mnemo')]
shutil.copytree('/home/admin/notes', V, ignore=skip, symlinks=False)
shutil.copytree('/home/admin/.claude/projects/-home-admin/memory', V + '/Agent Memory')
os.remove(V + '/Agent Memory/MEMORY.md') if os.path.exists(V + '/Agent Memory/MEMORY.md') else None
os.makedirs(V + '/Instructions', exist_ok=True)
m = json.load(open('../data/memory_items.json'))
n = 0
for i in m:
    if i['store'] == 'claudemd':
        with open(f"{V}/Instructions/{i['id'].replace(':','_')}.md", 'w') as f:
            f.write(f"# {i.get('section','')}\n\n{i['text']}\n"); n += 1
print('instruction notes', n, 'agent memory files', len(os.listdir(V + '/Agent Memory')))
