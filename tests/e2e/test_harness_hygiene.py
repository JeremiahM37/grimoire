"""Guards on the harness itself, so a fixed flake cannot quietly come back.

These read the test sources rather than driving a browser, so they cost
nothing and run in the same job as the suite they protect.
"""
import pathlib
import re

E2E = pathlib.Path(__file__).parent


def test_no_reload_races_the_boot():
    """A bare page.reload() followed by an interaction is a race.

    The app sets body[data-ready] at the END of boot, and boot restores the
    previously-open note. A click issued in between either misses a row that
    has not rendered or lands and is then overwritten by the restore. On CI
    that surfaced as test_alias_wikilink_navigates asserting on #title and
    getting "Trash E2E" -- a note belonging to a different module entirely --
    with the same commit passing on rerun.

    reload_ready() in conftest is the fix. This is what stops the next reload
    from being written the old way, because the failure it causes is rare,
    remote, and reads like someone else's problem.
    """
    offenders = []
    for path in sorted(E2E.glob("test_*.py")):
        lines = path.read_text().split("\n")
        for i, line in enumerate(lines):
            if not re.search(r"\b(page|pg)\.reload\(\)", line):
                continue
            following = "\n".join(lines[i + 1:i + 3])
            if "data-ready" not in following:
                offenders.append(f"{path.name}:{i + 1}: {line.strip()}")
    assert not offenders, (
        "these reloads race the app's boot — use reload_ready(page) from "
        "conftest, or wait for body[data-ready] on the next line:\n  "
        + "\n  ".join(offenders))
