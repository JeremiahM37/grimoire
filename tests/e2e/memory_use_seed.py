"""Demo data for the Memory use panel: injections, linked actions, reminders,
withheld decisions and compiled rule checks, written straight into the
adherence and rules databases of a running server's vault (the same approach
the usage dashboard test takes). Numbers are fixed so the panel can be asserted."""
import json
import sqlite3
import time
import urllib.request
from pathlib import Path

RULE = "note:Agent Memory/never-force-push.md"
TESTS = "note:Agent Memory/run-tests-before-commit.md"
FACT = ""  # set by seed(): the id the server gave the demo fact
THIN = "note:Projects/deploy-notes.md"
NOTES = {
    "Agent Memory/never-force-push.md": "---\nkind: rule\n---\nNever force-push to main; use --force-with-lease on a feature branch.\n",
    "Agent Memory/run-tests-before-commit.md": "---\nkind: rule\n---\nRun the test suite before every commit.\n",
    "Projects/deploy-notes.md": "# Deploy notes\n\nThe staging deploy runs from the release branch.\n",
}
EXPOSURES_RULE = 16


def _ensure(base):
    for path in ("/api/memory/adherence", "/api/memory/rules"):
        urllib.request.urlopen(base + path, timeout=10).read()


def seed(base, vault, now=None):
    """Seed a running server at BASE whose vault is VAULT; returns the targets."""
    now = int(now or time.time())
    for path, body in NOTES.items():
        req = urllib.request.Request(base + "/api/notes", method="POST", headers={"Content-Type": "application/json"},
                                     data=json.dumps({"path": path, "body": body}).encode())
        try:
            urllib.request.urlopen(req, timeout=10).read()
        except Exception:
            pass
    req = urllib.request.Request(base + "/api/memory", method="POST", headers={"Content-Type": "application/json"},
                                 data=json.dumps({"text": "The staging database is restored from the nightly snapshot at 03:00.",
                                                  "kind": "fact", "agent": "demo"}).encode())
    made = json.load(urllib.request.urlopen(req, timeout=10))
    global FACT
    FACT, fact_path = "fact:" + made["id"], made["path"]
    _ensure(base)
    db = sqlite3.connect(Path(vault) / ".grimoire" / "adherence.db", timeout=10)
    cols = ("session,tag,key,target,path,fact_id,stage,relevance,ts,tools,cited,check_result,contradicted,finalized,counted,"
            "fp_n,fp_hit,kind,rem,pend,ask,p_withhold,tool")

    def inject(session, tag, target, kind, ts, *, fp_hit=0, cited=0, check="", tools=1, rem="", pend="", tool="", p=0):
        path = target[5:] if target.startswith("note:") else fact_path
        fact = target[5:] if target.startswith("fact:") else ""
        cur = db.execute(f"INSERT INTO injections({cols}) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,1,1,3,?,?,?,?,0,?,?)",
                         (session, tag, tag + "0" * 28, target, path, fact, "action" if pend else "prompt", 0.8, ts, tools, cited, check,
                          fp_hit, kind, rem, pend, p, tool))
        return cur.lastrowid

    def act(session, seq, tool, ts, *, failed=0, reedit=0, tf=-1, tp=-1):
        cur = db.execute("INSERT INTO trace_actions(session,seq,tu,tool,target_hash,region,ts,stage,failed,tests_pass,tests_fail,reedit,revert,thrash,denied,correction)"
                         " VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,0,0,0)",
                         (session, seq, f"toolu_{session}_{seq}", tool, f"h{session}{seq}", "", ts, "early", failed, tp, tf, reedit))
        return cur.lastrowid

    def link(inj, action, evidence, ts):
        db.execute("INSERT OR IGNORE INTO trace_links(injection_id,action_id,evidence,ts) VALUES(?,?,?,?)", (inj, action, evidence, ts))

    # The rule memory: shown 16 times; most sessions pick it up and fail less.
    evidence = ["fp", "fp", "tag", "check", "changed", "fp"]
    for i in range(EXPOSURES_RULE):
        sid, ts = f"sess-rule-{i:02d}-aaaa", now - (EXPOSURES_RULE - i) * 3600
        picked = i % 4 != 3
        reminded = i < 4
        inj = inject(sid, "a1b2", RULE, "rule", ts, fp_hit=2 if picked else 0, cited=1 if i % 5 == 0 else 0, p=0.05 if i >= 11 else 0,
                     rem=("changed" if i % 2 else "unchanged") if reminded else "", pend="p" + str(i) if reminded else "", tool="Bash" if reminded else "")
        for seq in range(1, 7):
            linked = picked and seq <= 3
            bad = (seq in (2, 5)) if not linked else (i == 5 and seq == 3)
            a = act(sid, seq, "Edit" if seq % 2 else "Bash", ts + seq * 30, failed=1 if bad else 0, reedit=1 if (bad and seq == 5) else 0)
            if linked:
                link(inj, a, evidence[(i + seq) % len(evidence)], ts)
    # A memory that mostly gets ignored.
    for i in range(12):
        sid = f"sess-fact-{i:02d}-bbbb"
        inject(sid, "c3d4", FACT, "fact", now - i * 7200, fp_hit=1 if i < 2 else 0)
        for seq in range(1, 4):
            act(sid, seq, "Bash", now - i * 7200 + seq * 20, failed=1 if seq == 3 else 0)
    # A rule that gets violated.
    for i in range(9):
        sid = f"sess-test-{i:02d}-cccc"
        inject(sid, "e5f6", TESTS, "rule", now - i * 5400, check="violated" if i % 3 == 0 else "followed")
    # Too little to read.
    for i in range(2):
        inject(f"sess-thin-{i}-dddd", "0718", THIN, "fact", now - i * 600)
    for i in range(4):
        db.execute("INSERT INTO trace_withheld(session,tag,key,target,kind,stage,tool,relevance,p_withhold,ts) VALUES(?,?,?,?,?,?,?,?,?,?)",
                   (f"sess-held-{i}-eeee", "a1b2", "a1b2" + "0" * 28, RULE, "rule", "prompt", "", 0.8, 0.05, now - i * 600))
        for seq in range(1, 4):
            act(f"sess-held-{i}-eeee", seq, "Bash", now - i * 600 + seq * 20, failed=1 if (i == 0 and seq == 2) else 0)
    db.commit()
    db.close()

    rules = sqlite3.connect(Path(vault) / ".grimoire" / "rules.db", timeout=10)
    rules.execute("CREATE TABLE IF NOT EXISTS rule_checks (id TEXT PRIMARY KEY, target TEXT NOT NULL, body TEXT NOT NULL, "
                  "status TEXT NOT NULL, user_state TEXT NOT NULL DEFAULT '', updated INTEGER NOT NULL)")
    for rid, target, text, status, prec, lo, hi, labelled in (
        ("r1", RULE, "Never force-push to main", "enforce", 0.97, 0.88, 0.99, 31),
        ("r2", TESTS, "Run the test suite before every commit", "active", 0.92, 0.78, 0.97, 18),
    ):
        body = {"id": rid, "target": target, "rule_text": text, "spec": {"shape": "forbid", "action": "x"}, "status": status,
                "precision": prec, "ci95_low": lo, "ci95_high": hi, "labelled": labelled, "true_violations": int(prec * labelled), "matches": 40}
        rules.execute("INSERT OR REPLACE INTO rule_checks(id,target,body,status,user_state,updated) VALUES(?,?,?,?,?,?)",
                      (rid, target, json.dumps(body), status, "", now))
    rules.commit()
    rules.close()
    return {"rule": RULE, "tests": TESTS, "fact": FACT, "thin": THIN, "fact_path": fact_path}
