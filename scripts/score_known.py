#!/usr/bin/env python3
"""Vedetta - scores answers against docs/domande_risposta_nota.json.

The judge sees one answer, the pages it read and one item at a time. It never
learns which version of the system wrote the answer.

Usage:
  score_known.py --tags TAG [TAG ...] [--db PATH] [--estimate]
    bench rounds (runs_TAG.json or runs_TAG_N.json, suite "known"): each case that
    names a known entry in scripts/benchmark.json is judged on that entry; writes
    scripts/out/known_ROUND.json per round and prints the totals. The database is
    the bench's (~/vedetta-bench/data/vedetta.db). --estimate counts the characters
    a judging would send and calls no judge.
  score_known.py RUNS.json --db PATH/vedetta.db --out SCORES.json
    RUNS.json is a list of {"k": case id, "v": version, "round": n, "sid": session}.
Items added later can be scored without repeating the searches: already scored
pairs of session and item are kept.

An item with a "check" is decided by this script, not by the judge, and again at every
run. "addresses_on_pages" (a trap, with "hosts"): the answer gives an e-mail address
of those sites that no page it read on them shows. The judge sees only the addresses of the
pages read, not their text; the text comes from the round's cache.
"""
import argparse
import concurrent.futures as cf
import json
import os
import re
import sqlite3
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
SET = os.path.join(HERE, "..", "docs", "domande_risposta_nota.json")
OUT = os.path.join(HERE, "out")
BENCHMARK = os.path.join(HERE, "benchmark.json")
BENCH_DB = os.path.join(os.environ.get("VEDETTA_BENCH_DIR", os.path.expanduser("~/vedetta-bench")), "data", "vedetta.db")
CACHE_ROOT = os.path.dirname(BENCH_DB)
EMAIL_RE = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}")
ARCHIVE_RE = re.compile(r"^(?:https?://)?(?:www\.)?web\.archive\.org/web/[^/]+/", re.I)

SYSTEM = """You check one statement against one answer written for a user. Reply with a single JSON object and nothing else: {"verdict": "...", "quote": "the shortest passage of the answer that decided it, or empty"}.

For kind MUST, the statement is true and the verdict says what the answer does with it:
- RIGHT: the answer says it, in any words or language.
- WRONG: the answer says something on the same point that contradicts it.
- ABSENT: the answer does not address the point.

For kind TRAP, the statement describes a mistake and the verdict says whether the answer makes it:
- MADE: the answer makes this mistake.
- AVOIDED: the answer does not make it, including when it does not address the point.

Judge what the answer asserts. A mistake the answer reports only to warn against it is not made. Being long, confident or well written earns nothing. The list of pages read is given because some statements are about what was read."""


def config():
    conf = dict(os.environ)
    path = os.path.join(HERE, "judge.env")
    if os.path.exists(path):
        for line in open(path):
            m = re.match(r"\s*([A-Z_]+)\s*=\s*(.*?)\s*$", line)
            if m and m[1] not in conf:
                conf[m[1]] = m[2].strip("\"'")
    return conf["JUDGE_BASE_URL"].rstrip("/"), conf["JUDGE_MODEL"], conf.get("JUDGE_API_KEY", "")


def user_message(kind, item, answer, pages):
    return f"KIND: {kind}\nSTATEMENT: {item['t']}\n\nPAGES READ:\n{pages}\n\nANSWER:\n{answer}"


def request_chars(kind, item, answer, pages):
    """Characters one judge call sends (--estimate)."""
    return len(SYSTEM) + len(user_message(kind, item, answer, pages))


def ask(cfg, kind, item, answer, pages):
    base, model, key = cfg
    user = user_message(kind, item, answer, pages)
    body = {"model": model, "temperature": 0, "reasoning_effort": "low",
            "messages": [{"role": "system", "content": SYSTEM}, {"role": "user", "content": user}]}
    req = urllib.request.Request(base + "/chat/completions", json.dumps(body).encode(),
                                 {"Content-Type": "application/json", "Authorization": "Bearer " + key})
    allowed = {"must": ("RIGHT", "WRONG", "ABSENT"), "trap": ("MADE", "AVOIDED")}[kind]
    for _ in range(4):
        try:
            with urllib.request.urlopen(req, timeout=180) as resp:
                text = json.load(resp)["choices"][0]["message"]["content"] or ""
            out = json.loads(re.search(r"\{.*\}", text, re.S)[0])
            if out.get("verdict") in allowed:
                return out["verdict"], str(out.get("quote", ""))[:400]
        except Exception as err:  # one bad reply must not lose the whole round
            last = str(err)
    return "ERROR", locals().get("last", "bad reply")


def runs_from_tags(tags, out_dir=OUT, benchmark=BENCHMARK):
    """The runs of bench rounds: for each case that names a known entry, that entry,
    the round and the session. Cases without a known answer are left out."""
    import bench_report as br
    with open(benchmark) as f:
        bench = json.load(f)
    known = {c["id"]: c.get("known") for c in bench.get("cases", []) + bench.get("extra_cases", [])}
    runs = []
    for tag in tags:
        for name in br.resolve_rounds(tag, out_dir):
            with open(os.path.join(out_dir, f"runs_{name}.json")) as f:
                for case, sid in json.load(f).items():
                    if known.get(case):
                        runs.append({"k": known[case], "v": name, "round": name, "sid": sid})
    return runs


def split_known(runs, cases):
    """(runs whose entry is in the set, entries not in the set yet)."""
    ready = [r for r in runs if r["k"] in cases]
    missing = list(dict.fromkeys(r["k"] for r in runs if r["k"] not in cases))
    return ready, missing


def summary(done):
    """Totals per round: must items right/wrong/absent, traps made, judge errors."""
    out = {}
    for d in done:
        s = out.setdefault(str(d["round"]), dict(must=0, RIGHT=0, WRONG=0, ABSENT=0, trap=0, MADE=0, AVOIDED=0, errors=0))
        if d["verdict"] == "ERROR":
            s["errors"] += 1
            continue
        s[d["kind"]] += 1
        s[d["verdict"]] = s.get(d["verdict"], 0) + 1
    for s in out.values():
        s["pct_right"] = round(100.0 * s["RIGHT"] / s["must"], 1) if s["must"] else 0.0
    return out


def format_summary(summ):
    lines = []
    for name, s in summ.items():
        lines.append(f"  {name:16} must right {s['RIGHT']}/{s['must']} ({s['pct_right']:.1f}%), wrong {s['WRONG']}, "
                     f"absent {s['ABSENT']}; traps made {s['MADE']}/{s['trap']}" + (f"; judge errors {s['errors']}" if s["errors"] else ""))
    return "\n".join(lines)


def page_host(url):
    """Host of a page, without www; an archived copy counts as the site it copies."""
    url = ARCHIVE_RE.sub("", url.strip())
    if not re.match(r"^[a-z]+://", url, re.I):
        url = "https://" + url
    host = (urllib.parse.urlsplit(url).hostname or "").lower()
    return host[4:] if host.startswith("www.") else host


def on_site(host, hosts):
    return any(host == h or host.endswith("." + h) for h in hosts)


def addresses_on_pages(answer, pages, hosts):
    """Trap check: MADE when the answer gives an e-mail address of one of these sites
    that no page read on them shows. Addresses elsewhere (the writer's own, left to
    fill in) are not looked at. pages: (url, text) pairs."""
    texts = [text.lower() for url, text in pages if on_site(page_host(url), hosts)]
    found = dict.fromkeys(m.group(0).lower().rstrip(".") for m in EMAIL_RE.finditer(answer))
    missing = [a for a in found if on_site(a.rsplit("@", 1)[1], hosts) and not any(a in t for t in texts)]
    return ("MADE", ", ".join(missing)) if missing else ("AVOIDED", "")


CHECKS = {"addresses_on_pages": addresses_on_pages}


def session_pages(meta, cache_pages):
    """(url, text) of the pages a session read, from its read calls and the round's cache."""
    import eval as ev
    out = []
    for c in ((meta or {}).get("loop") or {}).get("calls_detail") or []:
        if c.get("tool") == "read" and c.get("url") and c.get("result") == "ok":
            doc = cache_pages.get(ev.norm_url(c["url"]))
            if doc is not None:
                out.append((c["url"], doc.get("content") or ""))
    return out


def round_pages(name, cache_root=CACHE_ROOT):
    """normalized URL -> document of a bench round's page cache, or None without one."""
    import free_report as fr
    d = fr.round_cache_dir(name, cache_root)
    return fr.load_cache(d)[1] if d else None


def last_answer(db, sid):
    row = db.execute("select content, meta_json from messages where session_id=? and role='assistant' "
                     "order by created_at desc limit 1", (sid,)).fetchone()
    if not row:
        return None
    body, _, notes = (row[0] or "").partition("<!-- vedetta:notes -->")
    return body.strip(), notes, json.loads(row[1] or "{}") or {}


def build_jobs(runs, cases, db, have):
    """Judge calls still to make; items with a machine check are left out."""
    jobs = []
    for run in runs:
        got = last_answer(db, run["sid"])
        if not got:
            continue
        body, notes, _ = got
        pages = "\n".join(re.findall(r"^\d+\. (\[.*?\]\(.*?\))\s*$", notes, re.M | re.S)) or "(none listed)"
        for kind in ("must", "trap"):
            for item in cases[run["k"]][kind]:
                if "check" not in item and (run["sid"], kind, item["t"]) not in have:
                    jobs.append((run, kind, item, body, pages))
    return jobs


def machine_verdicts(runs, cases, db, pages_of):
    """Verdicts of the items with a machine check. pages_of(round) gives the round's
    page cache, or None: then the verdict is ERROR (counted nowhere, tried again)."""
    out, caches = [], {}
    for run in runs:
        items = [(kind, item) for kind in ("must", "trap") for item in cases[run["k"]][kind] if "check" in item]
        got = last_answer(db, run["sid"]) if items else None
        if not got:
            continue
        body, _, meta = got
        if run["round"] not in caches:
            caches[run["round"]] = pages_of(run["round"])
        cache = caches[run["round"]]
        for kind, item in items:
            check = dict(item["check"])
            if cache is None:
                verdict, quote = "ERROR", "no page cache for this round"
            else:
                verdict, quote = CHECKS[check.pop("type")](body, session_pages(meta, cache), **check)
            out.append({"k": run["k"], "v": run["v"], "round": run["round"], "sid": run["sid"], "kind": kind,
                        "item": item["t"], "evidence": item["e"], "verdict": verdict, "quote": quote, "by": "machine"})
    return out


def score(runs, cases, db, out_path, cfg=None, pages_of=round_pages):
    """Judge the items not yet scored for these runs; keep the old verdicts of items
    still in the set (an item whose text changed is judged again). Items with a
    machine check are decided again every time. Returns every verdict of out_path."""
    current = {(k, kind, item["t"]) for k, c in cases.items() for kind in ("must", "trap") for item in c[kind]}
    checked = {(kind, item["t"]) for c in cases.values() for kind in ("must", "trap") for item in c[kind] if "check" in item}
    done = json.load(open(out_path)) if os.path.exists(out_path) else []
    done = [d for d in done if d["verdict"] != "ERROR" and (d["k"], d["kind"], d["item"]) in current
            and (d["kind"], d["item"]) not in checked]
    machine = machine_verdicts(runs, cases, db, pages_of)
    done += machine
    have = {(d["sid"], d["kind"], d["item"]) for d in done}
    jobs = build_jobs(runs, cases, db, have)
    if jobs:
        cfg = cfg or config()

        def work(job):
            run, kind, item, body, pages = job
            verdict, quote = ask(cfg, kind, item, body, pages)
            return {"k": run["k"], "v": run["v"], "round": run["round"], "sid": run["sid"], "kind": kind,
                    "item": item["t"], "evidence": item["e"], "verdict": verdict, "quote": quote}

        with cf.ThreadPoolExecutor(6) as pool:
            for res in pool.map(work, jobs):
                done.append(res)
    json.dump(done, open(out_path, "w"), ensure_ascii=False, indent=1)
    print(f"{out_path}: {len(jobs)} scored, {len(machine)} by machine, {sum(d['verdict'] == 'ERROR' for d in done)} errors")
    return done


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("runs", nargs="?", help="a list of runs (old use); with --tags, leave out")
    ap.add_argument("--tags", nargs="+", help="bench rounds, as for bench_report.py")
    ap.add_argument("--db", help="database of the sessions (default with --tags: the bench's)")
    ap.add_argument("--out", help="verdicts file (old use; with --tags: scripts/out/known_ROUND.json)")
    ap.add_argument("--estimate", action="store_true", help="count the characters a judging would send, call no judge")
    args = ap.parse_args()
    cases = {c["id"]: c for c in json.load(open(SET))["cases"]}
    if args.tags:
        runs, missing = split_known(runs_from_tags(args.tags), cases)
        if missing:
            print("no items in the set yet, not judged:", ", ".join(missing))
        rounds = list(dict.fromkeys(r["round"] for r in runs))
    else:
        if not (args.runs and args.db and args.out):
            ap.error("give RUNS --db --out, or --tags")
        runs, rounds = json.load(open(args.runs)), [None]
    db = sqlite3.connect(f"file:{args.db or BENCH_DB}?mode=ro", uri=True)
    db.text_factory = lambda b: b.decode("utf-8", "replace")
    everything = []
    for name in rounds:
        part = [r for r in runs if name is None or r["round"] == name]
        out_path = args.out if name is None else os.path.join(OUT, f"known_{name}.json")
        if args.estimate:
            have = set()
            if os.path.exists(out_path):
                have = {(d["sid"], d["kind"], d["item"]) for d in json.load(open(out_path)) if d["verdict"] != "ERROR"}
            jobs = build_jobs(part, cases, db, have)
            n = sum(request_chars(kind, item, body, pages) for _, kind, item, body, pages in jobs)
            print(f"  {name or out_path:16} {len(jobs):4} calls  {n / 1e6:.2f} million characters")
            continue
        everything += score(part, cases, db, out_path)
    if everything and args.tags:
        print(format_summary(summary(everything)))


if __name__ == "__main__":
    main()
