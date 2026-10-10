"""Python port of go/internal/bank/tokens.go CountTokens.

The server's own estimator is Go and lives in an internal package, so the
benchmark cannot call it directly. This is a line-for-line port. Its agreement
with the Go function is checked by `run.sh --check-tokens`, which compiles the
Go original from the repo and diffs counts over a fixed sample.

Known divergence: Go's unicode.Latin table is approximated by Latin-1 plus the
Latin Extended blocks. The corpus is ASCII, so the approximation cannot change a
benchmark number.
"""
from __future__ import annotations


def _is_latin(ch: str) -> bool:
    o = ord(ch)
    return o <= 0xFF or 0x100 <= o <= 0x24F or 0x1E00 <= o <= 0x1EFF


def count_tokens(s: str) -> int:
    n = 0
    i = 0
    size = len(s)
    while i < size:
        r = s[i]
        if r.isspace():
            i += 1
        elif r.isalpha():
            j = i
            while j < size and s[j].isalpha():
                j += 1
            letters = j - i
            if ord(r) > 0xFF and not _is_latin(r):
                n += letters  # non-Latin scripts: about a token per character
            else:
                n += 1 + (letters - 1) // 6
            i = j
        elif r.isdecimal():
            j = i
            while j < size and s[j].isdecimal():
                j += 1
            n += (j - i + 2) // 3
            i = j
        else:
            n += 1
            i += 1
    return n
