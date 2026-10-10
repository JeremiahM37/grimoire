"""Seeded synthetic memory corpus for the latency benchmark.

Everything here is a pure function of (seed, n): the same arguments produce
byte-identical vault files and the same query set on every machine. Facts are
written in the on-disk bullet format the server itself parses
(go/internal/memory/entry.go, Entry.Format), so the server indexes them through
its normal path rather than a shortcut.

Mix (per fact, all from the seeded stream):
  * entities: people (with a first-name alias), services (host-like names) and
    projects; each entity has 2-3 attributes, so a (entity, attribute) key is
    the unit of belief;
  * supersession: ~10% of facts re-state an existing key with a new value; the
    older belief is struck through and carries sup=<new id>;
  * authorship: ~25% human (by=human), the rest agent writes from a small pool;
  * validity: ~20% carry valid_from, a third of those also valid_to;
  * importance: ~20% unrated, the rest 1-5 weighted toward 3-4.
"""
from __future__ import annotations

import hashlib
import random
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta

CORPUS_VERSION = 1
FACTS_PER_NOTE = 50

FIRST = ["Dana", "Priya", "Marco", "Sam", "Lena", "Omar", "Ines", "Kofi", "Yuki", "Tomas",
         "Aisha", "Ravi", "Hana", "Felix", "Noor", "Diego", "Mei", "Gareth", "Zara", "Ilya",
         "Chloe", "Samir", "Nadia", "Ben", "Rosa", "Hugo", "Leah", "Arjun", "Iris", "Mateo"]
INITIALS = [chr(c) for c in range(ord("A"), ord("Z") + 1)]
LAST = ["Kim", "Sharma", "Diaz", "Okafor", "Novak", "Haddad", "Silva", "Mensah", "Tanaka",
        "Berg", "Rahman", "Iyer", "Moreau", "Schmidt", "Aziz", "Costa", "Lindqvist", "Adeyemi",
        "Fischer", "Quinn", "Ortiz", "Petrov", "Ng", "Walsh", "Khan", "Rossi", "Mori", "Dubois"]
SERVICES = ["pgbouncer", "grafana", "redis", "nginx", "postgres", "prometheus", "loki",
            "jellyfin", "sonarr", "radarr", "paperless", "immich", "homebox", "mealie",
            "gluetun", "qbittorrent", "authelia", "crowdsec", "uptime-kuma", "n8n", "searxng",
            "chroma", "ollama", "seafile", "syncthing", "restic", "tailscale", "cadvisor"]
HOST_ROLES = ["db", "app", "cache", "edge", "worker", "build", "media", "ai"]
PROJECTS = ["librarr", "gamarr", "sentinel", "grimoire", "lectern", "homelab-api", "doc-rag",
            "formwork", "mttyd", "terraform-status", "backup-status", "discord-bot"]
THINGS = ["release checklist", "PgBouncer config", "backup rotation", "on-call runbook",
          "certificate renewals", "DNS records", "media library", "indexer list",
          "Grafana dashboards", "incident review", "ingest pipeline", "photo albums",
          "invoice archive", "recipe import", "vault sync", "game library", "token rotation",
          "load test plan", "schema migrations", "alert routing"]
PREFS = ["tabs over spaces", "dark mode", "short standups", "async reviews", "Go over Python",
         "terse commit messages", "daily notes in the morning", "email digests weekly"]
TZS = ["UTC", "Europe/Berlin", "America/New_York", "Asia/Tokyo", "Australia/Sydney"]
AGENTS = ["claude-code", "codex", "homelab-ai", "lectern-worker", "librechat"]
HUMANS = ["jam", "dana", "priya", "marco"]
CATEGORIES = ["preference", "fact", "procedure", "status"]
IMPORTANCE_WEIGHTS = [(1, 5), (2, 15), (3, 40), (4, 25), (5, 15)]
BASE = datetime(2026, 1, 1, 0, 0, tzinfo=UTC)
SPAN_DAYS = 273  # Jan 1 .. end of Sep 2026; stamps increase with generation order


@dataclass
class Entity:
    eid: str
    kind: str          # person | service | project
    display: str
    alias: str


@dataclass
class Fact:
    id: str
    text: str
    entity: str
    attr: str
    question: str
    stamp: datetime
    agent: str
    human: bool
    category: str
    importance: int = 0
    valid_from: str = ""
    valid_to: str = ""
    superseded_by: str = ""
    note: str = ""
    seq: int = 0
    tags: dict = field(default_factory=dict)


def _id(seed: int, tag: str, i: int) -> str:
    return hashlib.sha256(f"{seed}:{tag}:{i}".encode()).hexdigest()[:12]


def _stamp(seq: int, n: int) -> datetime:
    return BASE + timedelta(days=SPAN_DAYS * seq / max(n, 1), minutes=seq % 1440)


def _entities(rng: random.Random, n_entities: int) -> list[Entity]:
    out: list[Entity] = []
    seen: set[str] = set()
    while len(out) < n_entities:
        r = rng.random()
        if r < 0.50:
            kind = "person"
            display = f"{rng.choice(FIRST)} {rng.choice(INITIALS)}. {rng.choice(LAST)}"
            if display in seen:
                continue
            alias = display.split()[0]
        elif r < 0.90:
            kind = "service"
            display = f"{rng.choice(SERVICES)}-{rng.randrange(100, 1000)}" if rng.random() < 0.5 \
                else f"{rng.choice(HOST_ROLES)}{rng.randrange(1, 60):02d}.prod"
            if display in seen:
                continue
            alias = display.split("-")[0].split(".")[0]
        else:
            kind = "project"
            display = rng.choice(PROJECTS)
            if display in seen:
                display = f"{display}-{rng.randrange(2, 500)}"
                if display in seen:
                    continue
            alias = display
        seen.add(display)
        out.append(Entity(eid=f"e{len(out):06d}", kind=kind, display=display, alias=alias))
    return out


ATTRS = {
    "person": [
        ("owns", "{e} owns the {thing}", "what does {a} own"),
        ("prefers", "{e} prefers {pref}", "what does {a} prefer"),
        ("tz", "{e} works in {tz}", "what timezone is {a} in"),
    ],
    "service": [
        ("port", "{e} listens on port {port}", "what port does {a} listen on"),
        ("host", "{e} runs on {host}", "where does {a} run"),
        ("version", "{e} is pinned to version {ver}", "what version is {a} pinned to"),
    ],
    "project": [
        ("release", "{e} targets release {ver}", "which release does {a} target"),
        ("repo", "{e} lives in {path}", "where does {a} live"),
    ],
}


def _value(rng: random.Random, kind: str, attr: str) -> dict:
    if attr == "owns":
        return {"thing": rng.choice(THINGS)}
    if attr == "prefers":
        return {"pref": rng.choice(PREFS)}
    if attr == "tz":
        return {"tz": rng.choice(TZS)}
    if attr == "port":
        return {"port": rng.randrange(1024, 65000)}
    if attr == "host":
        return {"host": f"{rng.choice(HOST_ROLES)}{rng.randrange(1, 60):02d}.prod"}
    if attr in ("version", "release"):
        return {"ver": f"{rng.randrange(1, 9)}.{rng.randrange(0, 30)}.{rng.randrange(0, 20)}"}
    if attr == "repo":
        return {"path": f"~/projects/{rng.choice(PROJECTS)}"}
    raise ValueError(attr)


def _make_fact(rng: random.Random, seed: int, tag: str, i: int, n: int,
               ent: Entity, attr: str, seq: int) -> Fact:
    tmpl, qtmpl = next((t, q) for a, t, q in ATTRS[ent.kind] if a == attr)
    vals = _value(rng, ent.kind, attr)
    # Use the alias for about a third of person/service mentions so retrieval
    # sees both the full name and the short form that people actually type.
    name = ent.alias if rng.random() < 0.33 else ent.display
    text = tmpl.format(e=name, **vals)
    human = rng.random() < 0.25
    agent = rng.choice(HUMANS) if human else rng.choice(AGENTS)
    category = rng.choice(CATEGORIES)
    importance = 0
    if rng.random() >= 0.20:
        importance = rng.choices([w for w, _ in IMPORTANCE_WEIGHTS],
                                 [wt for _, wt in IMPORTANCE_WEIGHTS])[0]
    valid_from = valid_to = ""
    if rng.random() < 0.20:
        start = datetime(2025, 6, 1, tzinfo=UTC) + timedelta(days=rng.randrange(0, 430))
        valid_from = start.strftime("%Y-%m-%d")
        if rng.random() < 1 / 3:
            valid_to = (start + timedelta(days=rng.randrange(30, 400))).strftime("%Y-%m-%d")
    # The question uses the unique display name; first-name aliases are ambiguous.
    return Fact(
        id=_id(seed, tag, i), text=text, entity=ent.eid, attr=attr,
        question=qtmpl.format(a=ent.display), stamp=_stamp(seq, n), agent=agent,
        human=human, category=category, importance=importance,
        valid_from=valid_from, valid_to=valid_to, seq=seq,
    )


def build(seed: int, n: int) -> tuple[list[Fact], list[Entity]]:
    """Return the n facts in generation order (stamps increase) and entities."""
    rng = random.Random(f"{seed}:corpus:{n}")
    entities = _entities(rng, max(60, n // 2))
    by_eid = {e.eid: e for e in entities}
    used: dict[tuple[str, str], Fact] = {}
    current: list[tuple[str, str]] = []  # keys whose belief is still current
    facts: list[Fact] = []
    while len(facts) < n:
        i = len(facts)
        # One decision per fact: ~10% of facts re-state a current key. A rejected
        # new-key draw below retries the same fact without re-rolling this coin.
        if current and rng.random() < 0.10:
            key = current[rng.randrange(len(current))]
            ent = by_eid[key[0]]
            fact = _make_fact(rng, seed, "upd", i, n, ent, key[1], i)
            old = used[key]
            old.superseded_by = fact.id
            current.remove(key)
        else:
            ent = entities[rng.randrange(len(entities))]
            attr = rng.choice([a for a, _, _ in ATTRS[ent.kind]])
            key = (ent.eid, attr)
            while key in used:  # keep one current belief per key: ground truth stays unambiguous
                ent = entities[rng.randrange(len(entities))]
                attr = rng.choice([a for a, _, _ in ATTRS[ent.kind]])
                key = (ent.eid, attr)
            fact = _make_fact(rng, seed, "new", i, n, ent, attr, i)
        used[key] = fact
        current.append(key)
        facts.append(fact)
    return facts, entities


def write_vault(root, facts: list[Fact]) -> None:
    """Write the facts as memory notes under root/memory/."""
    from pathlib import Path
    mem = Path(root) / "memory"
    mem.mkdir(parents=True, exist_ok=True)
    for k in range(0, len(facts), FACTS_PER_NOTE):
        chunk = facts[k:k + FACTS_PER_NOTE]
        lines = ["# Memory", ""]
        for f in chunk:
            lines.append(bullet(f))
        (mem / f"facts-{k // FACTS_PER_NOTE:05d}.md").write_text("\n".join(lines) + "\n")


def bullet(f: Fact) -> str:
    """Mirror of memory.Entry.Format for the fields the corpus uses."""
    stamp = f.stamp.strftime("%Y-%m-%d %H:%M")
    attribution = stamp + (f" · {f.agent}" if f.agent else "")
    body = f"**{attribution}** — {f.text}"
    if f.superseded_by:
        body = f"~~{body}~~"
    fields = [f"id={f.id}"]
    if f.category:
        fields.append(f"cat={f.category}")
    if f.human:
        fields.append("by=human")
    if f.superseded_by:
        fields.append(f"sup={f.superseded_by}")
    if f.importance:
        fields.append(f"imp={f.importance}")
    if f.valid_from:
        fields.append(f"valid_from={f.valid_from}")
    if f.valid_to:
        fields.append(f"valid_to={f.valid_to}")
    return f"- {body} <!--m {' '.join(fields)}-->"


def queries(facts: list[Fact], seed: int, k: int) -> list[Fact]:
    """k query targets drawn from beliefs that are still current."""
    rng = random.Random(f"{seed}:queries:{len(facts)}:{k}")
    current = [f for f in facts if not f.superseded_by]
    return [current[rng.randrange(len(current))] for _ in range(k)]


def write_stream(seed: int, n_existing: int, k: int, entities: list[Entity],
                 current_keys: list[tuple[str, str]]) -> list[dict]:
    """k remember() payloads: ~10% re-state an existing key (exercises
    reconcile's supersede path), the rest are new beliefs."""
    rng = random.Random(f"{seed}:writes:{n_existing}:{k}")
    by_eid = {e.eid: e for e in entities}
    out = []
    for i in range(k):
        if current_keys and rng.random() < 0.10:
            eid, attr = current_keys[rng.randrange(len(current_keys))]
            ent = by_eid[eid]
        else:
            ent = entities[rng.randrange(len(entities))]
            attr = rng.choice([a for a, _, _ in ATTRS[ent.kind]])
        f = _make_fact(rng, seed, "write", i, k, ent, attr, i)
        human = f.human
        out.append({
            # Spread over 50 topic notes (~4 facts each). Writing every fact into one
            # growing note makes each write cost grow with the note (re-index of the
            # whole note), which would measure note size, not the write path.
            "topic": f"bench-writes-{i % 50:02d}",
            "text": f.text,
            "agent": f.agent,
            "category": f.category,
            "human": human,
            **({"importance": f.importance} if f.importance else {}),
        })
    return out
