import json
import os
import subprocess
import sys
import urllib.request
from pathlib import Path


def test_real_hook_is_scoped_bounded_and_rehydrates_after_correction(knowledge_server, tmp_path):
    base_url, _, _ = knowledge_server

    def request(path, body):
        payload = json.dumps(body).encode()
        with urllib.request.urlopen(urllib.request.Request(
            base_url + path, data=payload, headers={"Content-Type": "application/json"},
        ), timeout=10) as response:
            return json.load(response)

    first = request("/api/memory", {
        "text": "Kestrel deployment uses copper certificates", "topic": "auto-kestrel",
        "human": True,
    })
    request("/api/memory", {
        "text": "Kestrel deployment unrelated project secret", "topic": "auto-other",
        "infer": False,
    })
    environment = {**os.environ, "GRIMOIRE_URL": base_url,
                   "GRIMOIRE_CONTEXT_MODE": "scoped",
                   "GRIMOIRE_CONTEXT_PATHS": json.dumps([first["path"]]),
                   "GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path),
                   "GRIMOIRE_CONTEXT_MAX_BYTES": "900"}
    environment.pop("GRIMOIRE_AUTH_TOKEN", None)
    script = Path(__file__).parents[2] / "clients/hooks/grimoire_context.py"

    def hook(prompt):
        result = subprocess.run([sys.executable, str(script)], input=json.dumps({
            "hook_event_name": "UserPromptSubmit", "session_id": "real-hook-test",
            "cwd": str(tmp_path), "prompt": prompt,
        }), text=True, capture_output=True, env=environment, check=True, timeout=5)
        return result.stdout

    output = json.loads(hook("Kestrel deployment certificates?"))
    context = output["hookSpecificOutput"]["additionalContext"]
    assert "copper" in context and "unrelated project secret" not in context
    assert len(context.encode()) <= 900
    assert hook("Kestrel deployment certificates?") == ""
    request("/api/memory", {
        "text": "Kestrel deployment uses silver certificates", "target_path": first["path"],
        "target_id": first["id"], "expected_text": "Kestrel deployment uses copper certificates",
        "human": True,
    })
    updated = hook("What certificates does Kestrel deployment use?")
    assert "silver" in updated and "copper" not in updated
    assert hook("continue") == ""
