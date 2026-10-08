#!/usr/bin/env python3
"""Graph view performance harness (Playwright). Usage:
  graph_bench.py BASE_URL [real|10000|50000] [--shot out.png] [--video dir]
Measures time-to-first-paint after opening the graph, then frame times and long
tasks (>50 ms) during 3 s of scripted wheel-zoom and drag-pan. Synthetic graphs
are served by intercepting /api/graph, so the vault is never touched."""
import json, random, sys, time
from playwright.sync_api import sync_playwright

def synth(n, m):
    r = random.Random(7); k = max(4, n // 150)
    comm = [r.randrange(k) for _ in range(n)]; by = [[] for _ in range(k)]
    for i, c in enumerate(comm): by[c].append(i)
    ids = [f"Folder {comm[i]}/note-{i}.md" for i in range(n)]
    seen = set(); edges = []
    while len(edges) < m:
        a = int(r.random() ** 2 * n)
        b = r.choice(by[comm[a]]) if r.random() < .85 else r.randrange(n)
        if a != b and (a, b) not in seen and (b, a) not in seen:
            seen.add((a, b)); edges.append((a, b))
    t0 = 1_600_000_000
    return ids, [f"Note {i}" for i in range(n)], [t0 + i * 20000 for i in range(n)], edges

def main():
    base = sys.argv[1]; mode = sys.argv[2] if len(sys.argv) > 2 else "real"
    shot = sys.argv[sys.argv.index("--shot") + 1] if "--shot" in sys.argv else None
    video = sys.argv[sys.argv.index("--video") + 1] if "--video" in sys.argv else None
    with sync_playwright() as p:
        # GL=hw uses the machine's GPU; the default is SwiftShader software GL, a worst case for fill-rate
        import os
        gl = ["--use-gl=angle", "--use-angle=gl-egl"] if os.environ.get("GL") == "hw" else ["--use-gl=angle", "--use-angle=swiftshader", "--enable-unsafe-swiftshader"]
        b = p.chromium.launch(args=gl + ["--ignore-gpu-blocklist"])
        ctx = b.new_context(viewport={"width": 1280, "height": 860}, record_video_dir=video, record_video_size={"width": 1280, "height": 860}) if video else b.new_context(viewport={"width": 1280, "height": 860})
        page = ctx.new_page()
        payload = {"bytes": 0}
        if mode != "real":
            n = int(mode); ids, titles, ts, edges = synth(n, n * 3)
            def handle(route):
                compact = "compact=1" in route.request.url
                body = ({"v": 2, "ids": ids, "titles": titles, "t": ts, "tags": [[] for _ in ids], "edges": [x for e in edges for x in e], "unresolved": []} if compact else
                        {"nodes": [{"id": i, "title": t} for i, t in zip(ids, titles)], "edges": [{"src": ids[a], "dst": ids[c]} for a, c in edges], "unresolved": []})
                s = json.dumps(body, separators=(",", ":")); payload["bytes"] = len(s)
                route.fulfill(status=200, content_type="application/json", body=s)
            page.route("**/api/graph*", handle)
        else:
            page.on("response", lambda r: payload.update(bytes=len(r.body())) if "/api/graph" in r.url else None)
        page.add_init_script("""window.__lt=[];window.__ltOn=true;new PerformanceObserver(l=>l.getEntries().forEach(e=>window.__lt.push([e.startTime,e.duration]))).observe({entryTypes:['longtask']});""")
        page.goto(base); page.wait_for_selector("body[data-ready]")
        page.evaluate("window.__lt.length=0")
        page.hover("#graph-open"); page.wait_for_timeout(400)  # a person hovers before clicking; that warms the chunk and the data
        page.evaluate("window.__lt.length=0")
        t0 = page.evaluate("performance.now()")
        page.evaluate("document.querySelector('#graph-open').click()")
        # all notes scope when the select exists, so the whole graph is painted
        page.wait_for_selector("#graph-canvas[data-zoom]", timeout=120000)
        try:  # the new engine marks its first rendered frame; the old 2D canvas has no marks, so data-zoom is its paint
            page.wait_for_function("performance.getEntriesByName('graph:frame').length>0", timeout=1500)
            fmp = page.evaluate("performance.getEntriesByName('graph:frame')[0].startTime") - t0
        except Exception:
            fmp = page.evaluate("performance.now()") - t0
        marks = page.evaluate("Object.fromEntries(performance.getEntriesByType('mark').filter(m=>m.name.startsWith('graph:')).map(m=>[m.name.slice(6),Math.round(m.startTime-%s)]))" % t0)
        try: page.select_option("#graph-scope", "all")
        except Exception: pass
        load_lt = page.evaluate("window.__lt.map(x=>Math.round(x[1]))"); ltAt = page.evaluate("window.__lt.map(x=>Math.round(x[0]-window.__t1))")
        page.wait_for_timeout(9000 if mode == '50000' else 5000)  # let the layout settle
        page.on("console", lambda m: print("console", m.text))
        if os.environ.get("EXPERIMENT"): page.evaluate(os.environ["EXPERIMENT"])
        if shot: page.screenshot(path=shot)
        page.evaluate("""window.__fr=[];window.__lt.length=0;window.__rec=true;window.__t1=performance.now();let l=performance.now();(function f(t){if(!window.__rec)return;window.__fr.push(t-l);l=t;requestAnimationFrame(f)})(l);""")
        box = page.locator("#graph-canvas").bounding_box(); cx, cy = box["x"] + box["width"] / 2, box["y"] + box["height"] / 2
        page.mouse.move(cx, cy)
        end = time.time() + 1.5
        while time.time() < end:
            page.mouse.wheel(0, -120); page.wait_for_timeout(30)
        while time.time() < end + 0.6:
            page.mouse.wheel(0, 140); page.wait_for_timeout(30)
        page.mouse.down()
        for i in range(60):
            page.mouse.move(cx + i * 4, cy + i * 2); page.wait_for_timeout(16)
        page.mouse.up()
        page.evaluate("window.__rec=false")
        fr = sorted(page.evaluate("window.__fr")); lt = page.evaluate("window.__lt.map(x=>Math.round(x[1]))"); ltAt = page.evaluate("window.__lt.map(x=>Math.round(x[0]-window.__t1))")
        avg = sum(fr) / len(fr)
        print(json.dumps({"mode": mode, "payload_kb": round(payload["bytes"] / 1024), "fmp_ms": round(fmp), "marks": marks, "load_longtasks": load_lt,
            "fps": round(1000 / avg, 1), "p95_frame_ms": round(fr[int(len(fr) * .95)]), "max_frame_ms": round(fr[-1]), "interaction_longtasks": lt, "at": ltAt, "slow": page.evaluate("window.__slow||[]")}))
        ctx.close(); b.close()
main()
