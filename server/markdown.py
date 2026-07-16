"""Markdown parsing: YAML-ish frontmatter, [[wiki-links]], #tags, title.

Intentionally dependency-light — a small, forgiving frontmatter parser (no yaml
lib required for the common key: value / list case) plus regexes for links/tags.
"""
import re

FRONTMATTER_RE = re.compile(r"^---\s*\n(.*?)\n---\s*\n?", re.DOTALL)
WIKILINK_RE = re.compile(r"\[\[([^\[\]|]+?)(?:\|([^\[\]]+))?\]\]")
# a #tag: word chars/hyphens/slashes, not inside a word, not a markdown heading
TAG_RE = re.compile(r"(?:^|(?<=\s))#([A-Za-z][\w/-]*)")
H1_RE = re.compile(r"^#\s+(.+?)\s*$", re.MULTILINE)
# a fenced code block or inline code — tags/links inside are ignored
CODE_FENCE_RE = re.compile(r"```.*?```|`[^`]*`", re.DOTALL)


def parse_frontmatter(text: str) -> tuple[dict, str]:
    """Return (frontmatter dict, body). Missing/blank frontmatter → ({}, text)."""
    m = FRONTMATTER_RE.match(text)
    if not m:
        return {}, text
    fm = _parse_yamlish(m.group(1))
    return fm, text[m.end():]


def _parse_yamlish(block: str) -> dict:
    out: dict = {}
    key = None
    for raw in block.split("\n"):
        line = raw.rstrip()
        if not line.strip():
            continue
        if re.match(r"^\s*-\s+", line) and key is not None:
            out.setdefault(key, [])
            if isinstance(out[key], list):
                out[key].append(_scalar(re.sub(r"^\s*-\s+", "", line)))
            continue
        m = re.match(r"^([A-Za-z0-9_\-]+):\s*(.*)$", line)
        if not m:
            continue
        key, val = m.group(1), m.group(2).strip()
        if val == "":
            out[key] = []          # a list follows, or an empty value
        elif val.startswith("[") and val.endswith("]"):
            inner = val[1:-1].strip()
            out[key] = [_scalar(x.strip()) for x in inner.split(",")] if inner else []
        else:
            out[key] = _scalar(val)
    # collapse empty-list placeholders that never got items into ""
    return {k: (v if v != [] or _was_list(block, k) else "") for k, v in out.items()}


def _was_list(block: str, key: str) -> bool:
    return bool(re.search(rf"^{re.escape(key)}:\s*\n\s*-\s", block, re.MULTILINE)) or \
        bool(re.search(rf"^{re.escape(key)}:\s*\[", block, re.MULTILINE))


def _scalar(v: str):
    v = v.strip().strip('"').strip("'")
    if v.lower() in ("true", "false"):
        return v.lower() == "true"
    return v


def _strip_code(body: str) -> str:
    return CODE_FENCE_RE.sub(" ", body)


def extract_links(body: str) -> list[dict]:
    """Wiki-links as [{target, alias}] — code spans ignored, order preserved, deduped."""
    seen, out = set(), []
    for m in WIKILINK_RE.finditer(_strip_code(body)):
        target = m.group(1).strip()
        # drop a #heading or ^block anchor for link *resolution*
        base = target.split("#", 1)[0].split("^", 1)[0].strip() or target
        if base.lower() in seen:
            continue
        seen.add(base.lower())
        out.append({"target": base, "alias": (m.group(2) or "").strip()})
    return out


def extract_tags(body: str) -> list[str]:
    stripped = _strip_code(body)
    # ignore markdown headings (## foo) — those aren't tags
    stripped = re.sub(r"^#{1,6}\s.*$", "", stripped, flags=re.MULTILINE)
    seen, out = set(), []
    for m in TAG_RE.finditer(stripped):
        t = m.group(1)
        if t.lower() not in seen:
            seen.add(t.lower())
            out.append(t)
    return out


def derive_title(frontmatter: dict, body: str, filename_stem: str) -> str:
    if frontmatter.get("title"):
        return str(frontmatter["title"])
    h1 = H1_RE.search(body)
    if h1:
        return h1.group(1).strip()
    return filename_stem
