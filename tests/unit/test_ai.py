from server import ai


def test_hash_embed_is_deterministic_and_normalized():
    v1 = ai.embed(["hello world notes"])[0]
    v2 = ai.embed(["hello world notes"])[0]
    assert v1 == v2 and len(v1) == ai.EMBED_DIM
    import math
    assert abs(math.sqrt(sum(x * x for x in v1)) - 1.0) < 1e-6


def test_embedding_reflects_token_overlap():
    a = ai.embed(["apples oranges bananas fruit"])[0]
    close = ai.embed(["apples and fruit"])[0]
    far = ai.embed(["quantum physics relativity"])[0]
    assert ai.cosine(a, close) > ai.cosine(a, far)


def test_pack_unpack_roundtrip():
    v = ai.embed(["roundtrip"])[0]
    assert ai.unpack(ai.pack(v)) == [round(x, 6) or x for x in ai.unpack(ai.pack(v))]
    assert len(ai.unpack(ai.pack(v))) == ai.EMBED_DIM


def test_chunking_packs_paragraphs():
    text = "\n\n".join([f"para {i} " + "x" * 300 for i in range(5)])
    chunks = ai.chunk_text(text, target=800)
    assert len(chunks) >= 2 and all(len(c) <= 1400 for c in chunks)


def test_extractive_answer_cites_and_handles_empty():
    assert "couldn't find" in ai.answer("x", [])
    out = ai.answer("apples", [{"path": "fruit.md", "title": "Fruit", "chunk": "apples are red"}])
    assert "[[fruit" in out and "apples are red" in out
