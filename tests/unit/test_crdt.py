"""Sequence CRDT — correctness + convergence proofs."""
import random

from server.crdt import BASE, Doc, key_between


def _clone(doc, site):
    return Doc.from_json(doc.to_json(), site)


# ---- fractional keys --------------------------------------------------------

def test_key_between_is_strictly_ordered():
    a, b = (), (BASE,)
    prev = a
    for _ in range(200):                       # repeatedly insert at the front-ish
        k = key_between(prev, b)
        assert prev < k < b
        prev = k


def test_key_between_dense_insertion():
    left, right = (1,), (2,)
    k = key_between(left, right)
    assert left < k < right                    # can always fit between adjac*ent* ints


# ---- basic editing ----------------------------------------------------------

def test_from_text_roundtrip():
    doc = Doc.from_text("the quick brown fox", "s")
    assert doc.text() == "the quick brown fox"


def test_local_edit_insert_delete_replace():
    doc = Doc.from_text("hello world", "s")
    doc.local_edit("hello brave world")        # insert
    assert doc.text() == "hello brave world"
    doc.local_edit("hi brave world")           # replace start
    assert doc.text() == "hi brave world"
    doc.local_edit("hi brave")                 # delete end
    assert doc.text() == "hi brave"


def test_serialization_roundtrip():
    doc = Doc.from_text("data ✓ unicode ☃", "s")
    doc.local_edit("data ✓ unicode ☃ edited")
    again = Doc.from_json(doc.to_json(), "s")
    assert again.text() == doc.text()


# ---- CRDT algebra: commutative, idempotent, associative ---------------------

def test_merge_idempotent():
    a = Doc.from_text("abc", "A")
    before = a.text()
    a.merge(_clone(a, "A"))
    assert a.text() == before


def test_merge_commutative():
    base = Doc.from_text("shared base", "seed")
    a = _clone(base, "A"); a.local_edit("shared base + A")
    b = _clone(base, "B"); b.local_edit("B! shared base")
    ab = _clone(a, "A").merge(_clone(b, "B"))
    ba = _clone(b, "B").merge(_clone(a, "A"))
    assert ab.text() == ba.text()              # order-independent


def test_concurrent_edits_both_survive_and_converge():
    base = Doc.from_text("hello world", "seed")
    a = _clone(base, "A"); a.local_edit("hello brave world")   # insert in middle
    b = _clone(base, "B"); b.local_edit("hello world!!!")      # append
    ab = _clone(a, "A").merge(_clone(b, "B"))
    ba = _clone(b, "B").merge(_clone(a, "A"))
    assert ab.text() == ba.text()
    assert "brave" in ab.text() and ab.text().endswith("!!!")  # neither edit lost


def test_concurrent_insert_and_delete_converge():
    base = Doc.from_text("keep this text", "seed")
    a = _clone(base, "A"); a.local_edit("keep text")           # delete " this"
    b = _clone(base, "B"); b.local_edit("keep this text now")  # append " now"
    ab = _clone(a, "A").merge(_clone(b, "B"))
    ba = _clone(b, "B").merge(_clone(a, "A"))
    assert ab.text() == ba.text()
    assert "this" not in ab.text() and ab.text().endswith("now")


def test_associativity_three_replicas():
    base = Doc.from_text("x", "seed")
    a = _clone(base, "A"); a.local_edit("Ax")
    b = _clone(base, "B"); b.local_edit("xB")
    c = _clone(base, "C"); c.local_edit("xCx")
    # (a·b)·c
    left = _clone(a, "A").merge(_clone(b, "B")).merge(_clone(c, "C"))
    # a·(b·c)
    bc = _clone(b, "B").merge(_clone(c, "C"))
    right = _clone(a, "A").merge(bc)
    assert left.text() == right.text()


# ---- randomized fuzz: any interleaving converges ---------------------------

def test_fuzz_convergence():
    rng = random.Random(1234)
    for trial in range(25):
        base = Doc.from_text("the initial shared document text", "seed")
        replicas = [_clone(base, s) for s in ("A", "B", "C")]
        # each replica makes a few independent random edits
        for r in replicas:
            for _ in range(rng.randint(1, 4)):
                t = r.text()
                if not t:
                    t = "x"
                pos = rng.randint(0, len(t))
                if rng.random() < 0.5 and len(t) > 3:      # delete a slice
                    end = min(len(t), pos + rng.randint(1, 3))
                    r.local_edit(t[:pos] + t[end:])
                else:                                       # insert
                    r.local_edit(t[:pos] + rng.choice("XYZ!? ") + t[pos:])
        # merge in two different random orders → must converge
        order1 = replicas[:]; rng.shuffle(order1)
        order2 = replicas[:]; rng.shuffle(order2)
        m1 = _clone(order1[0], "M1")
        for r in order1[1:]:
            m1.merge(_clone(r, r.site))
        m2 = _clone(order2[0], "M2")
        for r in order2[1:]:
            m2.merge(_clone(r, r.site))
        assert m1.text() == m2.text(), f"diverged on trial {trial}"
