# Prompt-cache-stable memory prefix

LLM prompt caches reuse work only when the leading bytes of a prompt are
identical to the leading bytes of an earlier one. Anything that moves early in
the prompt, such as a re-ranked list of recalled facts, throws away the cache
from that point on.

`GET /api/memory/prefix` returns the durable memories as an append-only log
with a fixed byte layout. Put it at the start of the system prompt, behind a
cache breakpoint. Put volatile, query-dependent recall after it.

## What goes in the block

Membership: a fact is included when it is

- live (not superseded and not expired),
- not disputed (`challenges` unset),
- not pulled from outside (untrusted origin),
- not private (private notes are never in this block), and
- either written by a person, or effectively importance 3 or more (an unrated
  agent fact counts as 3).

Order: first-write stamp, then id. Recall score, access counts and importance
changes never reorder anything, so a fact that was already in the block stays
where it was.

Format: a fixed header, then one line per fact:

```
# Durable memory (append-only log)
- [mem:ID] 2026-10-01 10:00 the team uses postgres
- [mem:ID] 2026-10-02 08:30 deploys go out on tuesdays
```

The header and lines hold no counts, no request times and no scores. Anything
of that kind would change on every write and sit in front of the cache.

## Stability rules

| Change to memory | Effect on the block |
|---|---|
| A new fact, written after every existing one | Appended at the end. Earlier bytes are unchanged. |
| A new fact that falls past the budget | Nothing visible changes. The block is byte-identical. |
| A fact superseded, forgotten, expired or disputed | Removed from its place. Everything after it moves. The reply is `rewritten=true`. |
| An earlier fact edited in place | Its line changes, and so does everything after it. `rewritten=true`. |

Caveat: stamps have minute resolution. Two facts written in the same minute are
ordered by id, so a fact written in the same minute as the newest one can land
just before it. That is a one-time rewrite, not a repeated one.

## Budget

`max_tokens` (default 2000, range 50 to 20000) bounds the block, using the same
token estimator as the rest of the memory engine. The block is cut from the
end only: the facts that fit are the oldest ones, and the newest fall off the
tail. While the block is full, the head is byte-stable.

The reply reports `bytes`, `tokens`, `entries` (in the block), `total` (all
qualifying facts), `omitted` and `truncated`.

Budget and cache: a larger budget is a larger cacheable prefix, and the cache
only pays off once the prefix is large enough for the provider to cache. Pick
the budget once per deployment and leave it alone. Changing it rewrites the
block.

## Calls

```
GET /api/memory/prefix                          full block
GET /api/memory/prefix?since=TOKEN              only what changed since TOKEN
GET /api/memory/prefix?max_tokens=N
```

Reply fields:

- `version`: sha256 (first 16 bytes, hex) of the block bytes. Equal bytes give
  an equal version, so it works as a cache key.
- `append_only_since`: the chain token at the end of the block. Pass this back
  as `since` next time. (Passing `version` also works, and gives `unchanged`
  while the block is unchanged.)
- `block`: the full block. It is present when `since` is absent, and when
  `rewritten` is true.
- `unchanged`: nothing new is visible under the budget. No `delta`.
- `delta`: the lines after the caller's token, when they are a pure append.
  `appended` counts them.
- `rewritten`: an earlier line changed or was removed, or the token is not
  part of this log. The caller must replace its cached block with `block`.

The token is a hash chain over the lines. The server does not store any state:
it finds the caller's token in the current chain. If it is found, the lines
before it are exactly the lines the caller holds. If it is not found, the
caller's log is not a prefix of this one.

Also available as the MCP tool `memory_prefix` (arguments `since`,
`max_tokens`) and as `grimoire memory prefix [--since TOKEN] [--max-tokens N]`.
The CLI prints the block to stdout. With `--since` it prints the delta, or the
word `unchanged` on stderr, so it can be redirected into a file.

## Placing it in a system prompt

### Anthropic (explicit cache breakpoints)

The prompt is cached in the order tools, then system, then messages. A
`cache_control` marker on a block caches everything up to and including that
block. Keep the stable parts first and mark the end of each stable part.

```python
system = [
    {"type": "text", "text": STATIC_INSTRUCTIONS,            # changes rarely
     "cache_control": {"type": "ephemeral"}},
    {"type": "text", "text": memory_block,                   # from /api/memory/prefix
     "cache_control": {"type": "ephemeral"}},
]
messages = [
    {"role": "user", "content": [
        {"type": "text", "text": f"Recall for this request:\n{recall_results}"},  # volatile, not cached
        {"type": "text", "text": user_message},
    ]},
]
```

Notes:

- Up to four breakpoints per request. Two are used above; a third can go after
  a large tool list if there is one.
- When the memory block changes, the cache is missed from that block onwards,
  and the static instructions above it still hit.
- The default lifetime is short (about five minutes, refreshed on each hit).
  The minimum cacheable length depends on the model. Check the current
  prompt-caching documentation before relying on a particular threshold.
- Keep the memory block's text exactly as received. Re-serialising it (JSON
  re-indenting, trimming trailing newlines) breaks the match.

### OpenAI (automatic prefix caching)

OpenAI caches automatically for prompts above a minimum length, matching in
fixed-size increments from the start. There are no markers, so the only lever is
order: static instructions first, the memory block second, the per-request
recall and the user message last.

```python
messages = [
    {"role": "system", "content": STATIC_INSTRUCTIONS + "\n\n" + memory_block},
    {"role": "user", "content": f"Recall for this request:\n{recall_results}\n\n{user_message}"},
]
```

Do not put a timestamp, request id or recall result inside the system message.
Any of those ends the shared prefix at that byte. The `prompt_cache_key`
parameter can pin requests to a cache shard for the same block; check the
current API reference for how it is supported.

## Suggested client loop

```python
token = None
block = None
while True:
    r = get("/api/memory/prefix", params={"since": token} if token else {})
    if r.get("rewritten") or "block" in r:
        block = r["block"]              # replace the cached copy
    elif r.get("delta"):
        block += r["delta"]             # pure append
    # unchanged: keep block as it is
    token = r["append_only_since"]
    system_prompt = STATIC_INSTRUCTIONS + "\n\n" + block
```

Check `version` against a hash of your own copy if you want to be sure the
append was applied exactly. A mismatch means the local copy has drifted: drop
`token` and fetch the full block.

## Tests

`go/internal/api/memory_prefix_test.go` covers:

- byte-identical output across repeated calls, including with the input
  reversed and recall-only fields changed;
- a pure append: the old block is a byte prefix of the new one, and `since`
  returns a one-line delta;
- `unchanged` at the current token and at the current version;
- supersession, in-place edits and unknown tokens all reporting `rewritten`;
- budget truncation from the end: a byte prefix of the full block, on a line
  boundary, within the token budget, and unchanged by facts that do not fit;
- selection (superseded, disputed, pulled, expired and low-importance agent
  facts are out; human facts are in);
- the HTTP surface, including 400s for bad budgets.

The route is probed by `TestLeakprobeRouteCoverage` (see
`docs/SECURITY-LEAKPROBE.md`).
