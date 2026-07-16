"""v0.4: audio memos (pluggable transcription) + browser/share capture."""


def test_audio_memo_transcribes_and_creates_note(client, monkeypatch):
    from server import ai
    monkeypatch.setattr(ai, "transcribe", lambda data, fn="": "hello from the memo")
    r = client.post("/api/audio", files={"file": ("memo.webm", b"\x00fakeaudio", "audio/webm")})
    assert r.status_code == 201
    j = r.json()
    assert j["transcript"] == "hello from the memo"
    assert j["path"].startswith("inbox/") and j["audio"].startswith("attachments/")
    # the note is real and searchable by its transcript
    note = client.get(f"/api/notes/{j['path']}").json()
    assert "hello from the memo" in note["body"] and "audio" in note["tags"]
    assert client.get("/api/search", params={"q": "memo"}).json()


def test_audio_attachment_written_to_vault(client, vaultdir, monkeypatch):
    from server import ai
    monkeypatch.setattr(ai, "transcribe", lambda d, fn="": "x")
    r = client.post("/api/audio", files={"file": ("m.webm", b"AUDIOBYTES", "audio/webm")}).json()
    apath = vaultdir / r["audio"]
    assert apath.exists() and apath.read_bytes() == b"AUDIOBYTES"


def test_transcription_failure_still_saves_memo(client, monkeypatch):
    from server import ai
    monkeypatch.setattr(ai, "transcribe", lambda d, fn="": "[transcription failed: boom]")
    r = client.post("/api/audio", files={"file": ("m.webm", b"x", "audio/webm")})
    assert r.status_code == 201 and "failed" in r.json()["transcript"]


def test_browser_capture_creates_note_with_source(client):
    r = client.post("/api/capture", json={
        "text": "an interesting article excerpt", "title": "Cool Article",
        "url": "https://example.test/post", "source": "extension"})
    assert r.status_code == 201
    note = client.get(f"/api/notes/{r.json()['path']}").json()
    assert "example.test" in note["body"] and note["frontmatter"]["source"] == "extension"
