"""A stand-in for an OpenAI-compatible chat server, for the memory-bank e2e tests.

It answers the calls a bank makes — fact extraction, consolidation, the
reflect tool loop and its helpers — with canned but input-driven JSON, so the
whole retain → consolidate → mental model → reflect path runs against a real
server without a real model. Every request is recorded (``StubLLM.calls``) so
a test can check what the engine asked for.

What it answers, by the system prompt the engine sends:

- extraction: one fact per sentence of the chunk, entities = capitalised words;
- consolidation: a new fact that shares three or more content words with an
  observation it was shown updates the closest one (citing the fact),
  anything else creates one;
- reflect turns: a retrieval call until something has been retrieved, then
  ``done`` citing every id the transcript holds, with an answer quoting the
  texts it saw and a counter, so every refresh writes a different answer;
- directive checks comply; a synthesis or rewrite echoes the evidence.
"""
import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STOP = {"the", "a", "an", "and", "or", "of", "to", "in", "on", "at", "for", "is", "was", "are", "were", "be",
        "by", "with", "that", "this", "it", "its", "as", "from", "has", "have", "had", "user", "they", "their"}


def words(text):
    return {w for w in re.findall(r"[a-z0-9]+", text.lower()) if w not in STOP and len(w) > 2}


def sentences(text):
    out = []
    for line in text.splitlines():
        line = re.sub(r"^\s*[\w .-]{1,40}(\[[^\]]*\])?:\s+", "", line)  # "speaker [time]: text"
        for s in re.split(r"(?<=[.!?])\s+", line.strip()):
            if len(s.split()) >= 3:
                out.append(s.strip())
    return out


class StubLLM:
    def __init__(self):
        self.calls = []
        self.answers = 0
        self.lock = threading.Lock()
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), self._handler())
        self.port = self.server.server_address[1]
        self.base_url = f"http://127.0.0.1:{self.port}/v1"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self):
        self.thread.start()
        return self

    def stop(self):
        self.server.shutdown()
        self.server.server_close()

    def kinds(self):
        with self.lock:
            return [c["kind"] for c in self.calls]

    # ---- replies -----------------------------------------------------------

    def reply(self, system, user):
        if system.startswith("You read one piece") or system.startswith("You annotate one piece"):
            return "extract", json.dumps({"facts": self.extract(user)})
        if system.startswith("You keep a memory bank's observations"):
            return "consolidate", json.dumps(self.consolidate(user))
        if system.startswith("You answer questions from what one memory bank holds"):
            return "reflect", json.dumps(self.reflect_turn(user))
        if system.startswith("You check an answer against a list of rules"):
            return "directives", json.dumps({"complies": True, "violations": []})
        if system.startswith("You write the final answer"):
            return "synthesize", self.answer(user, [])
        if system.startswith("Rewrite the text"):
            return "rewrite", user[-2000:]
        return "other", "{}"

    def extract(self, user):
        # The chunk follows a "Content:" line, after the date and context headers.
        body = user.split("\nContent:\n", 1)[-1]
        facts = []
        for s in sentences(body):
            ents = sorted({w for w in re.findall(r"\b[A-Z][a-z]+\b", s) if w.lower() not in STOP and w not in ("The", "She", "He", "They", "We", "It")})
            facts.append({"what": s, "when": "N/A", "where": "N/A", "who": "N/A", "why": "N/A",
                          "fact_kind": "conversation", "occurred_start": None, "occurred_end": None,
                          "fact_type": "world", "entities": ents or ["user"]})
        return facts

    def consolidate(self, user):
        new = re.findall(r"^\[([^\]]+)\] (.+?)(?: \((?:occurred|mentioned|authority)=.*\))?$", user.split("## Existing observations")[0], re.M)
        existing = []
        if "## Existing observations" in user:
            raw = user.split("## Existing observations", 1)[1].strip()
            try:
                existing = json.loads(raw)
            except ValueError:
                existing = []
        creates, updates, updated = [], [], set()
        for fid, text in new:
            # The observation sharing the most content words (three or more) is "the same thing".
            match, best = None, 2
            for o in existing:
                shared = len(words(o["text"]) & words(text))
                if o["id"] not in updated and shared > best:
                    match, best = o, shared
            quote = " ".join(text.split()[:4])
            if match:
                updated.add(match["id"])
                updates.append({"observation_id": match["id"], "text": f"{match['text']} Also: {text}",
                                "source_fact_ids": [fid], "evidence": [{"fact_id": fid, "quote": quote}], "reason": "extends"})
            else:
                creates.append({"text": text, "source_fact_ids": [fid], "evidence": [{"fact_id": fid, "quote": quote}], "reason": "new"})
        return {"creates": creates, "updates": updates, "deletes": []}

    def reflect_turn(self, user):
        ids = list(dict.fromkeys(re.findall(r'"id":\s*"([^"]+)"', user)))
        if not ids:
            q = re.search(r"## Question\s*\n+(.+)", user)
            return {"tool": "recall", "args": {"query": q.group(1).strip() if q else "everything"}}
        mem = [i for i in ids if not i.startswith("o")]
        obs = [i for i in ids if i.startswith("o")]
        return {"tool": "done", "args": {"answer": self.answer(user, ids), "memory_ids": mem, "observation_ids": obs}}

    def answer(self, user, ids):
        with self.lock:
            self.answers += 1
            n = self.answers
        texts = list(dict.fromkeys(re.findall(r'"text":\s*"([^"]{8,})"', user)))[:3]
        cited = " ".join(f"[{i}]" for i in ids[:4])
        return f"Answer {n}: " + (" ".join(texts) if texts else "nothing recorded.") + (f" {cited}" if cited else "")

    # ---- HTTP --------------------------------------------------------------

    def _handler(self):
        stub = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):  # keep test output quiet
                pass

            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length) or b"{}")
                if not self.path.endswith("/chat/completions"):
                    self.send_response(404)
                    self.end_headers()
                    return
                system = "\n".join(m.get("content", "") for m in body.get("messages", []) if m.get("role") == "system")
                user = "\n".join(m.get("content", "") for m in body.get("messages", []) if m.get("role") == "user")
                kind, content = stub.reply(system, user)
                with stub.lock:
                    stub.calls.append({"kind": kind, "system": system[:200], "user": user, "reply": content})
                out = {"id": f"stub-{len(stub.calls)}", "object": "chat.completion", "model": body.get("model", "stub"),
                       "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": "stop"}],
                       "usage": {"prompt_tokens": len(user) // 4, "completion_tokens": len(content) // 4,
                                 "total_tokens": (len(user) + len(content)) // 4}}
                raw = json.dumps(out).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        return Handler
