#!/bin/sh
# Install the latest Grimoire release for this machine.
#
#   curl -fsSL https://raw.githubusercontent.com/JeremiahM37/grimoire/main/install.sh | sh
#
# Picks the archive for this OS/CPU from the latest GitHub release, verifies it
# against checksums.txt, unpacks it whole (the console and plugins are files
# beside the binary, which finds them there) into /usr/local/share/grimoire or
# ~/.local/share/grimoire, and links `grimoire` and `grimoire-mcp` into the
# matching bin directory. GRIMOIRE_VERSION=vX.Y.Z pins a release;
# GRIMOIRE_INSTALL_PREFIX overrides the prefix. No service is started.
set -eu

repo="JeremiahM37/grimoire"
version="${GRIMOIRE_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  mingw*|msys*|cygwin*) echo "On Windows use PowerShell: irm https://raw.githubusercontent.com/$repo/main/install.ps1 | iex" >&2; exit 1 ;;
  *) echo "unsupported OS: $os" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac
if [ "$version" = latest ]; then base="https://github.com/$repo/releases/latest/download"
else base="https://github.com/$repo/releases/download/$version"; fi
archive="grimoire_${os}_${arch}.tar.gz"

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
echo "Downloading $archive ($version)…"
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
else got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

prefix="${GRIMOIRE_INSTALL_PREFIX:-}"
if [ -z "$prefix" ]; then
  if [ -w /usr/local/bin ] && [ -w /usr/local/share ] 2>/dev/null; then prefix=/usr/local
  elif command -v sudo >/dev/null 2>&1 && [ -d /usr/local ]; then prefix=/usr/local
  else prefix="$HOME/.local"; fi
fi
share="$prefix/share/grimoire"; bin="$prefix/bin"
# Decide once whether writing the prefix needs sudo: it does when the prefix
# (or, if it does not exist yet, its parent) is not ours to write.
probe="$prefix"; while [ ! -e "$probe" ]; do probe=$(dirname "$probe"); done
if [ -w "$probe" ]; then run() { "$@"; }; else echo "Installing under $prefix needs sudo."; run() { sudo "$@"; }; fi
run mkdir -p "$share" "$bin"
# A fresh tree each time: stale console assets from an older release would
# otherwise sit beside the new ones.
run rm -rf "$share.new"; run mkdir -p "$share.new"
run tar xzf "$tmp/$archive" -C "$share.new"
run rm -rf "$share"; run mv "$share.new" "$share"
for b in grimoire grimoire-mcp; do run ln -sfn "$share/$b" "$bin/$b"; done
echo "Installed $("$bin/grimoire" version) to $share (linked in $bin)"
case ":$PATH:" in *":$bin:"*) ;; *) echo "Note: $bin is not on your PATH." ;; esac
echo "Next: 'GRIMOIRE_VAULT=~/notes grimoire' serves your vault at http://localhost:9111; see https://github.com/$repo#quick-start"
