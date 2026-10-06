// Grimoire addresses notes relative to the folder it serves; Obsidian
// addresses them relative to the vault. They are the same folder in the
// common case, and `folder` covers the other one: Grimoire serving a
// subfolder of the Obsidian vault.

function clean(p: string): string {
  return p.replace(/\\/g, '/').replace(/^\/+|\/+$/g, '')
}

export function toVaultPath(grimoirePath: string, folder: string): string {
  const f = clean(folder)
  const p = clean(grimoirePath)
  return f ? `${f}/${p}` : p
}

/** The Grimoire path for a vault file, or null when it is outside the served folder. */
export function toGrimoirePath(vaultPath: string, folder: string): string | null {
  const f = clean(folder)
  const p = clean(vaultPath)
  if (!f) return p
  return p.startsWith(f + '/') ? p.slice(f.length + 1) : null
}

/** The memory topic a vault file holds, if it is a memory note. */
export function memoryTopic(vaultPath: string, folder: string): string | null {
  const g = toGrimoirePath(vaultPath, folder)
  if (!g) return null
  const m = /^memory\/(.+)\.md$/.exec(g)
  return m ? m[1] : null
}
