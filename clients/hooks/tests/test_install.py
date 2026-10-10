import importlib.util
import json
from pathlib import Path

import pytest

SPEC = importlib.util.spec_from_file_location(
    "grimoire_hook_install", Path(__file__).parents[1] / "install.py"
)
install = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(install)

EXISTING = {
    "model": "x",
    "hooks": {
        "SessionStart": [{"hooks": [{"type": "command", "command": "other-tool hook"}]}],
        "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "guard"}]}],
    },
}


@pytest.fixture
def target(tmp_path):
    path = tmp_path / "settings.json"
    path.write_text(json.dumps(EXISTING))
    path.chmod(0o640)
    return path


def run(*args):
    return install.main([str(a) for a in args])


def test_dry_run_is_the_default_and_changes_nothing(target, capsys):
    before = target.read_text()
    assert run(target) == 0
    assert target.read_text() == before
    assert list(target.parent.glob("*backup*")) == []
    out = capsys.readouterr().out
    assert "grimoire_context.py" in out and "dry run" in out


def test_apply_merges_without_disturbing_existing_entries(target):
    assert run(target, "--apply", "--mode", "all") == 0
    merged = json.loads(target.read_text())
    assert merged["model"] == "x"
    assert merged["hooks"]["PreToolUse"] == EXISTING["hooks"]["PreToolUse"]
    start = merged["hooks"]["SessionStart"]
    assert start[0] == EXISTING["hooks"]["SessionStart"][0]
    command = start[1]["hooks"][0]["command"]
    assert command.startswith("GRIMOIRE_CONTEXT_MODE=all python3 ")
    assert command.endswith("grimoire_context.py")
    assert start[1]["hooks"][0]["timeout"] == 3
    assert len(merged["hooks"]["UserPromptSubmit"]) == 1
    assert target.stat().st_mode & 0o777 == 0o640


def test_apply_is_idempotent_and_writes_one_backup(target, capsys):
    run(target, "--apply")
    once = target.read_text()
    backups = list(target.parent.glob("settings.json.grimoire-backup-*"))
    assert len(backups) == 1
    assert json.loads(backups[0].read_text()) == EXISTING
    capsys.readouterr()
    assert run(target, "--apply") == 0
    assert target.read_text() == once
    assert len(list(target.parent.glob("settings.json.grimoire-backup-*"))) == 1
    assert "nothing to do" in capsys.readouterr().out


def test_recognises_an_existing_install_of_the_script(tmp_path):
    path = tmp_path / "hooks.json"
    entry = {"hooks": [{"type": "command", "command": "A=1 /usr/bin/python3 /x/grimoire_context.py"}]}
    path.write_text(json.dumps({"hooks": {e: [entry] for e in install.EVENTS}}))
    before = path.read_text()
    assert run(path, "--apply") == 0
    assert path.read_text() == before


def test_creates_a_missing_file(tmp_path):
    path = tmp_path / ".codex" / "hooks.json"
    assert run(path, "--apply") == 0
    assert set(json.loads(path.read_text())["hooks"]) == set(install.EVENTS)


def test_refuses_malformed_input_and_secrets(tmp_path, capsys):
    path = tmp_path / "settings.json"
    path.write_text("[1]")
    assert run(path, "--apply") == 2
    path.write_text('{"hooks": {"SessionStart": {}}}')
    assert run(path, "--apply") == 2
    path.write_text("{not json")
    assert run(path, "--apply") == 2
    assert path.read_text() == "{not json"
    with pytest.raises(SystemExit):
        run(path, "--env", "GRIMOIRE_AUTH_TOKEN=abc")


def test_env_and_paths_are_shell_quoted(target):
    run(target, "--apply", "--env", 'GRIMOIRE_CONTEXT_PATHS=["a b.md"]')
    command = json.loads(target.read_text())["hooks"]["UserPromptSubmit"][0]["hooks"][0]["command"]
    assert "GRIMOIRE_CONTEXT_PATHS='[\"a b.md\"]'" in command
