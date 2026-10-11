"""Ten scripted project-convention tasks for the task-benefit study.

Every project here is FICTIONAL ("Quarry", plus a "Harbor" distractor). Each
convention is an arbitrary value that no reader could infer from the question
or from pretraining, so a memory-off answer can only be right by luck. That is
the point of the design, and it is also its limit: real conventions are not
arbitrary, so these tasks measure whether a stored fact is retrieved and used,
not how often memory helps on real work.

Each task has:
  fact     the memory entry written to the server (plain sentence)
  question the task the reader answers
  require  regexes that must ALL match the answer (case-insensitive)
  gold     the correct value, for the report and the dry-run oracle

DISTRACTORS are Harbor facts written to the same server. They share vocabulary
with the Quarry questions on purpose, so retrieval has to pick the right
project, and a retrieval miss shows up as a wrong answer.
"""

TASKS = [
    {"id": "T01", "gold": "make verify-quarry",
     "fact": "Quarry test suite: the full test suite is run with the command make verify-quarry.",
     "question": "What exact command runs the full Quarry test suite?",
     "require": [r"make\s+verify-quarry"]},
    {"id": "T02", "gold": "rel/QX-<major>.<minor>",
     "fact": "Quarry release branches follow the pattern rel/QX-<major>.<minor>, for example rel/QX-3.2.",
     "question": "What branch naming pattern does Quarry use for release branches?",
     "require": [r"rel/QX-"]},
    {"id": "T03", "gold": "quarry.blob_cache_root",
     "fact": "Quarry config: the blob cache directory is set by the key quarry.blob_cache_root.",
     "question": "Which Quarry config key sets the blob cache directory?",
     "require": [r"blob_cache_root"]},
    {"id": "T04", "gold": "4417",
     "fact": "Quarry dev server: the local dev server listens on port 4417.",
     "question": "What port does the Quarry local dev server listen on?",
     "require": [r"\b4417\b"]},
    {"id": "T05", "gold": "tessel-lint + .tessel.toml",
     "fact": "Quarry lint: run tessel-lint, configured by .tessel.toml at the repo root.",
     "question": "Which linter does Quarry use, and which file configures it?",
     "require": [r"tessel-lint", r"\.tessel\.toml"]},
    {"id": "T06", "gold": "storage-guild",
     "fact": "Quarry review rule: changes under db/migrations need two approvals from the storage-guild team.",
     "question": "Which team must approve Quarry changes under db/migrations?",
     "require": [r"storage-guild"]},
    {"id": "T07", "gold": "six-digit zero-padded prefix",
     "fact": "Quarry migrations use a six-digit zero-padded numeric prefix, such as 000417_add_index.sql.",
     "question": "How are Quarry migration files numbered?",
     "require": [r"six[- ]digit|6[- ]digit|000\d{3}"]},
    {"id": "T08", "gold": "QUARRY_TRACE_LEVEL",
     "fact": "Quarry logging: the trace log level is controlled by the environment variable QUARRY_TRACE_LEVEL.",
     "question": "Which environment variable sets the Quarry trace log level?",
     "require": [r"QUARRY_TRACE_LEVEL"]},
    {"id": "T09", "gold": "Thursdays at 14:00 UTC",
     "fact": "Quarry release schedule: releases are cut on Thursdays at 14:00 UTC.",
     "question": "On which weekday and time does Quarry cut its releases?",
     "require": [r"thursday", r"14:00"]},
    {"id": "T10", "gold": "qry-ticket:",
     "fact": "Quarry commit convention: commit messages begin with the ticket tag qry-ticket: followed by the ticket id.",
     "question": "What tag must Quarry commit messages begin with?",
     "require": [r"qry-ticket"]},
]

DISTRACTORS = [
    "Harbor test suite: the full Harbor test suite is run with the command make harbor-check.",
    "Harbor release branches follow the pattern hb/release-<major>, for example hb/release-7.",
    "Harbor config: the cache directory is set by the key harbor.cache_dir.",
    "Harbor dev server: the local dev server listens on port 8630.",
    "Harbor review rule: changes under schema/ need one approval from the platform-team.",
]
