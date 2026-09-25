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
if [ -n "${GRIMOIRE_RELEASE_BASE:-}" ]; then base="$GRIMOIRE_RELEASE_BASE"
elif [ "$version" = latest ]; then base="https://github.com/$repo/releases/latest/download"
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

prefix="${GRIMOIRE_INSTALL_PREFIX:-$HOME/.local}"
share="$prefix/share/grimoire"; bin="$prefix/bin"
# Decide once whether writing the prefix needs sudo: it does when the prefix
# (or, if it does not exist yet, its parent) is not ours to write.
probe="$prefix"; while [ ! -e "$probe" ]; do probe=$(dirname "$probe"); done
if [ -w "$probe" ]; then run() { "$@"; }; else echo "Installing under $prefix needs sudo."; run() { sudo "$@"; }; fi
run mkdir -p "$share" "$bin"
# A fresh tree each time: stale console assets from an older release would
# otherwise sit beside the new ones.
stage="$share.new.$$"
run mkdir -p "$stage"
run tar xzf "$tmp/$archive" -C "$stage"
[ -x "$stage/grimoire" ] && [ -x "$stage/grimoire-mcp" ] && [ -f "$stage/web/index.html" ] || {
  echo "Archive is incomplete; existing installation kept." >&2; exit 1;
}
previous="$share.previous.$$"
if [ -e "$share" ] || [ -L "$share" ]; then run mv "$share" "$previous"; fi
if ! run mv "$stage" "$share"; then
  if [ -e "$previous" ]; then run mv "$previous" "$share"; fi
  exit 1
fi
if [ -e "$previous" ]; then echo "Previous installation retained at $previous"; fi
for b in grimoire grimoire-mcp; do run ln -sfn "$share/$b" "$bin/$b"; done
echo "Installed $("$bin/grimoire" version) to $share (linked in $bin)"
case ":$PATH:" in *":$bin:"*) ;; *) echo "Add to your shell profile: export PATH=\"$bin:\$PATH\"" ;; esac
printf 'Next: GRIMOIRE_VAULT="$HOME/notes" "%s/grimoire" serve\nOpen http://localhost:9111\n' "$bin"
