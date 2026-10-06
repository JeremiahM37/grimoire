#!/bin/sh
# Install the plugin into an Obsidian vault: ./install.sh ~/path/to/vault
#
# Builds it first if main.js is missing. Then enable "Grimoire Agent Memory"
# under Settings → Community plugins (Obsidian asks once whether to trust it).
set -eu
vault="${1:?usage: ./install.sh /path/to/obsidian/vault}"
here="$(cd "$(dirname "$0")" && pwd)"
[ -d "$vault/.obsidian" ] || { echo "$vault is not an Obsidian vault (no .obsidian folder)" >&2; exit 1; }
if [ ! -f "$here/main.js" ]; then
  (cd "$here" && npm ci && npm run build)
fi
dest="$vault/.obsidian/plugins/grimoire-memory"
mkdir -p "$dest"
cp "$here/main.js" "$here/manifest.json" "$here/styles.css" "$dest/"
echo "Installed to $dest"
echo "Enable it in Obsidian: Settings → Community plugins → Grimoire Agent Memory"
