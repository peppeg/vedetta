#!/usr/bin/env python3
"""Compare fallback readers on the reader bench (R7.3).

For every page in data/reader-bench/manifest.json (written by reader_bench.py),
read it with each candidate and measure whether it was read (>= 250 characters
and no anti-bot marker), how many of the verbatim fact quotes are found in the
text (same normalization as pdf_compare.py), and the time:
  http        Vedetta's http reader (today's first reader, for reference)
  jina        Vedetta's Jina reader against the running Jina (today's fallback)
  chromium    Chromium headless through chromedp (scripts/browsercmp), HTML -> Vedetta's extractor
  lightpanda  Lightpanda through chromedp, HTML -> Vedetta's extractor
  browser     Vedetta's own browser reader against BROWSER_CDP_URL (SPEC_BROWSER B.5)
The browsers run once per candidate, in their own cgroup, to read their peak
memory. The text stored by the rounds' reader is scored as reference.

Usage:
  python3 scripts/reader_compare.py --chromium PATH --lightpanda PATH [--engines ...]
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile
import time

from pdf_compare import score

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
ENGINES = ["lightpanda", "chromium", "jina", "http", "browser"]


def build(tmp):
    tools = {"browsercmp": os.path.join(tmp, "browsercmp"), "htmltext": os.path.join(tmp, "htmltext")}
    subprocess.run(["go", "build", "-o", tools["browsercmp"], "."], cwd=os.path.join(HERE, "browsercmp"), check=True)
    subprocess.run(["go", "build", "-o", tools["htmltext"], "./scripts/htmltext"], cwd=REPO, check=True)
    return tools


def htmltext(tools, mode, src, out):
    proc = subprocess.run([tools["htmltext"], mode, src, out], capture_output=True, text=True, timeout=120)
    try:
        return json.loads(proc.stdout.strip().splitlines()[-1])
    except (IndexError, ValueError):
        return {"chars": 0, "error": proc.stderr.strip()[:200] or "no output"}


def run_browser(tools, engine, binary, urls_file, out_dir):
    """Read all URLs with one browser start; returns per-url results, summary and peak MB."""
    unit = f"r7-{engine}-{os.getpid()}"
    script = (f"{tools['browsercmp']} -engine {engine} -bin {binary} -urls {urls_file} -out {out_dir} "
              f"2>{out_dir}.err; cat /sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/memory.peak")
    proc = subprocess.run(["systemd-run", "--user", "--scope", "--quiet", "--collect", f"--unit={unit}",
                           "-p", "MemoryAccounting=yes", "bash", "-c", script],
                          capture_output=True, text=True, timeout=3600)
    pages, summary, peak = {}, {}, None
    for line in proc.stdout.splitlines():
        line = line.strip()
        if line.startswith("{"):
            d = json.loads(line)
            if "url" in d:
                pages[d["url"]] = d
            else:
                summary = d
        elif line.isdigit():
            peak = int(line) >> 20
    return pages, summary, peak


def is_read(info):
    return info.get("chars", 0) >= 250 and not info.get("block") and not info.get("error")


def jina_memory():
    proc = subprocess.run(["podman", "stats", "--no-stream", "--format", "{{.MemUsage}}", "jina-reader"],
                          capture_output=True, text=True)
    return proc.stdout.strip().split(" / ")[0] or "?"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bench", default=os.path.join(REPO, "data", "reader-bench"))
    ap.add_argument("--chromium", help="needed for the chromium engine")
    ap.add_argument("--lightpanda", help="needed for the lightpanda engine")
    ap.add_argument("--engines", nargs="+", default=ENGINES[:4], choices=ENGINES)
    args = ap.parse_args()

    with open(os.path.join(args.bench, "manifest.json")) as f:
        pages = json.load(f)["pages"]
    tmp = tempfile.mkdtemp(prefix="readercmp-")
    tools = build(tmp)
    urls_file = os.path.join(args.bench, "urls.txt")
    results = {p["url"]: {} for p in pages}
    runs = {}

    for engine in args.engines:
        out_dir = os.path.join(args.bench, engine)
        os.makedirs(out_dir, exist_ok=True)
        start = time.monotonic()
        if engine in ("chromium", "lightpanda"):
            binary = args.chromium if engine == "chromium" else args.lightpanda
            per_url, summary, peak = run_browser(tools, engine, binary, urls_file, out_dir)
            runs[engine] = {"start_ms": summary.get("start_ms"), "peak_mb": peak}
            for p in pages:
                b = per_url.get(p["url"], {"error": "no result"})
                info = {"ms": b.get("ms", 0), "error": b.get("error", "")}
                if b.get("file"):
                    t = htmltext(tools, "-html", b["file"], os.path.join(out_dir, f"{p['id']}.txt"))
                    info.update(chars=t.get("chars", 0), block=t.get("block", ""), title=t.get("title", ""))
                    info["error"] = info["error"] or t.get("error", "")
                results[p["url"]][engine] = info
        else:
            before = jina_memory() if engine == "jina" else None
            for p in pages:
                t = htmltext(tools, f"-{engine}", p["url"], os.path.join(out_dir, f"{p['id']}.txt"))
                results[p["url"]][engine] = {"ms": t.get("ms", 0), "chars": t.get("chars", 0), "block": t.get("block", ""),
                                             "title": t.get("title", ""), "error": t.get("error", "")}
            runs[engine] = {"memory_before": before, "memory_after": jina_memory()} if engine == "jina" else {}
        runs[engine]["wall_s"] = round(time.monotonic() - start, 1)
        print(f"{engine}: done in {runs[engine]['wall_s']} s {runs[engine]}", flush=True)

    totals = {}
    for p in pages:
        group = "control" if p["control"] else "fallback"
        for engine, info in results[p["url"]].items():
            path = os.path.join(args.bench, engine, f"{p['id']}.txt")
            text = open(path, errors="replace").read() if os.path.exists(path) and is_read(info) else ""
            info["exact"], info["cover"] = score(text, p["quotes"]) if p["quotes"] else (0, 0.0)
            t = totals.setdefault((group, engine), {"pages": 0, "read": 0, "exact": 0, "cover": 0.0, "ms": 0})
            t["pages"] += 1
            t["read"] += is_read(info)
            t["exact"] += info["exact"]
            t["cover"] += info["cover"]
            t["ms"] += info["ms"]
        if p["reference"]:
            text = open(os.path.join(args.bench, p["reference"]), errors="replace").read()
            ex, cov = score(text, p["quotes"]) if p["quotes"] else (0, 0.0)
            t = totals.setdefault((group, "reference"), {"pages": 0, "read": 0, "exact": 0, "cover": 0.0, "ms": 0})
            t["pages"] += 1
            t["read"] += len(text.strip()) >= 250
            t["exact"] += ex
            t["cover"] += cov

    for group in ("fallback", "control"):
        gp = [p for p in pages if p["control"] == (group == "control")]
        nq = sum(len(p["quotes"]) for p in gp)
        print(f"\n{group}: {len(gp)} pages, {nq} quotes")
        for engine in args.engines + ["reference"]:
            t = totals.get((group, engine))
            if t:
                avg = f"  avg {t['ms'] / t['pages'] / 1000:.1f} s/page" if engine != "reference" else ""
                print(f"  {engine:<11} read {t['read']:>3}/{t['pages']}  quotes exact {t['exact']:>3}/{nq}  "
                      f"cover {t['cover']:.1f}{avg}")
    print(f"\nruns: {json.dumps(runs)}")
    with open(os.path.join(args.bench, "compare.json"), "w") as f:
        json.dump({"results": results, "runs": runs,
                   "totals": {f"{g}/{e}": t for (g, e), t in totals.items()}}, f, indent=2)


if __name__ == "__main__":
    sys.exit(main())
