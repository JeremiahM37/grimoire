"""A true sequence CRDT for note text (conflict-free replicated document).

Model: a Logoot/fractional-index sequence CRDT. Every character is an *atom* with
a globally-unique, totally-ordered identifier

    id = (key, site, clock)

where `key` is a fractional-index digit path (a tuple of ints) that densely orders
atoms, and `(site, clock)` makes the id unique and breaks ties between concurrent
inserts. Deletes are tombstones. The document state is the set of live atoms.

Merge is a state-based join (union of atoms, union of tombstones, minus any
tombstoned atom). That join is **commutative, associative, and idempotent**, so
all replicas that see the same set of edits converge to the identical text,
regardless of the order edits arrive — no conflict copies for text. Proven by the
convergence tests in tests/unit/test_crdt.py.
"""
from __future__ import annotations

import difflib
import json

BASE = 1 << 16   # digit radix for fractional keys


def key_between(a: tuple, b: tuple) -> tuple:
    """Return a fractional key strictly between keys `a` and `b` (a < b).
    Keys compare lexicographically; a shorter key that is a prefix of a longer one
    sorts first (missing digit = -1 lower bound)."""
    out = []
    i = 0
    while True:
        da = a[i] if i < len(a) else -1
        db = b[i] if i < len(b) else BASE
        if db - da > 1:
            out.append((da + db) // 2)
            return tuple(out)
        out.append(da if da >= 0 else 0)
        i += 1


class Doc:
    """A replicated text document. `site` identifies this replica."""

    def __init__(self, site: str = "local"):
        self.site = site
        self.clock = 0
        self.atoms: dict[tuple, str] = {}   # id -> char
        self.tombs: set[tuple] = set()

    # ---- ordering -----------------------------------------------------------
    @staticmethod
    def _sortkey(atom_id: tuple):
        key, site, clock = atom_id
        return (key, site, clock)

    def _ordered_ids(self) -> list:
        return sorted(self.atoms, key=self._sortkey)

    def text(self) -> str:
        return "".join(self.atoms[i] for i in self._ordered_ids())

    # ---- editing ------------------------------------------------------------
    def _new_id(self, left_key: tuple, right_key: tuple) -> tuple:
        self.clock += 1
        return (key_between(left_key, right_key), self.site, self.clock)

    def _insert_run(self, ids: list, left_idx: int, chars: str) -> None:
        """Insert `chars` between visible position left_idx-1 and left_idx."""
        left_key = ids[left_idx - 1][0] if left_idx - 1 >= 0 else ()
        right_key = ids[left_idx][0] if left_idx < len(ids) else (BASE,)
        prev = left_key
        for ch in chars:
            aid = self._new_id(prev, right_key)
            self.atoms[aid] = ch
            prev = aid[0]

    def local_edit(self, new_text: str) -> None:
        """Reconcile a full-text replacement (from a file/editor) into CRDT ops."""
        ids = self._ordered_ids()
        old_text = "".join(self.atoms[i] for i in ids)
        if old_text == new_text:
            return
        sm = difflib.SequenceMatcher(None, old_text, new_text, autojunk=False)
        for tag, i1, i2, j1, j2 in sm.get_opcodes():
            if tag == "equal":
                continue
            if tag in ("delete", "replace"):
                for k in range(i1, i2):
                    self.tombs.add(ids[k])
                    self.atoms.pop(ids[k], None)
            if tag in ("insert", "replace"):
                self._insert_run(ids, i1, new_text[j1:j2])

    # ---- merge (the CRDT join) ---------------------------------------------
    def merge(self, other: Doc) -> Doc:
        self.tombs |= other.tombs
        for aid, ch in other.atoms.items():
            if aid not in self.tombs:
                self.atoms.setdefault(aid, ch)
        # drop anything the union of tombstones now covers
        for aid in list(self.atoms):
            if aid in self.tombs:
                del self.atoms[aid]
        # advance our clock past any observed to avoid future id collisions
        for aid in other.atoms.keys() | other.tombs:
            if aid[1] == self.site:
                self.clock = max(self.clock, aid[2])
        return self

    # ---- serialization ------------------------------------------------------
    def to_json(self) -> str:
        return json.dumps({
            "site": self.site, "clock": self.clock,
            "atoms": [[list(k), s, c, ch] for (k, s, c), ch in self.atoms.items()],
            "tombs": [[list(k), s, c] for (k, s, c) in self.tombs],
        }, separators=(",", ":"))

    @classmethod
    def from_json(cls, data: str, site: str = "local") -> Doc:
        d = json.loads(data)
        doc = cls(site)
        doc.clock = d.get("clock", 0)
        doc.atoms = {(tuple(k), s, c): ch for k, s, c, ch in d.get("atoms", [])}
        doc.tombs = {(tuple(k), s, c) for k, s, c in d.get("tombs", [])}
        return doc

    @classmethod
    def from_text(cls, text: str, site: str = "local") -> Doc:
        doc = cls(site)
        doc.local_edit(text)
        return doc
