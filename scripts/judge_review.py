#!/usr/bin/env python3
"""Vedetta - what the model did with the sentences the sentinel sent back (PIANO_DEEP_A_CICLO §13.4).

For every Deep a ciclo answer whose draft was sent back (meta_json.loop.reviewed),
the judge gets each sentence sent back, the details flagged in it, the page
passage Vedetta attached (if any) and the final answer, and says what became of
the flagged details in the final answer:

  KEPT       stated again as in the draft
  CORRECTED  replaced with a different value or name
  HEDGED     kept but marked as uncertain or not verified
  REMOVED    no longer in the final answer

Usage:
  python3 scripts/judge_review.py TAG [TAG ...]
Writes scripts/out/judgereview_ROUND.json and prints the totals, split by
sentences sent back with a passage (details seen only in previews) and without
(details found nowhere). Stdlib only.
"""
import argparse
import json
import os
import re
import sqlite3
import sys
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import bench_report as br  # noqa: E402
import judge  # noqa: E402
import judge_pages as jp  # noqa: E402

KINDS = ("KEPT", "CORRECTED", "HEDGED", "REMOVED")

SYSTEM_PROMPT = """You compare a draft with the final answer that replaced it.
You get the FINAL ANSWER and a numbered list of SENTENCES from the draft, each with the DETAILS (numbers, names) a checker flagged in it and, for some, a PASSAGE of a web page about it.
For each sentence, say what the final answer does with the flagged details:
- KEPT: it states them again as in the draft (other wording is fine).
- CORRECTED: it states a different value or name in their place.
- HEDGED: it keeps them but says they are uncertain, approximate or not verified.
- REMOVED: they are no longer in the final answer.
The texts may be in different languages. Answer with one line per sentence, exactly "n: KIND | what changed in a few words", and nothing else."""

LINE_RE = re.compile(r"^\s*\**\s*(\d+)\s*\**\s*[:.)\-]\s*\**\s*(KEPT|CORRECTED|HEDGED|REMOVED)\b\**\s*(?:[|:\-]\s*(.*))?$", re.I | re.M)


def item_text(n, it):
    details = (it.get("numbers") or []) + (it.get("names") or []) + (it.get("preview") or [])
    s = f"{n}. {it['sentence']}\n   DETAILS: {', '.join(details)}"
    if it.get("passage"):
        s += f"\n   PASSAGE ({it.get('url', '')}): {it['passage']}"
    return s


def classify(ask, final, items):
    out = {}
    for start in range(0, len(items), jp.BATCH):
        chunk = items[start:start + jp.BATCH]
        msg = "FINAL ANSWER:\n" + final + "\n\nSENTENCES:\n" + "\n".join(item_text(start + i + 1, it) for i, it in enumerate(chunk))
        for m in LINE_RE.finditer(ask({"message": msg}) or ""):
            out[int(m.group(1))] = (m.group(2).upper(), (m.group(3) or "").strip())
    return [out.get(i + 1, ("ERROR", "")) for i in range(len(items))]


def review_round(name, conn, ask, workers):
    with open(os.path.join(jp.OUT, f"runs_{name}.json")) as f:
        runs = json.load(f)
    jobs = []
    for case, sid in runs.items():
        turn = jp.load_turn(conn, sid)
        loop = (turn or {}).get("meta", {}).get("loop") or {}
        if loop.get("reviewed") and loop.get("review_items"):
            jobs.append((case, sid, jp.ev.answer_body(turn["content"] or ""), loop["review_items"]))
    with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        results = list(pool.map(lambda j: classify(ask, j[2], j[3]), jobs))
    cases = {}
    for (case, sid, _, items), kinds in zip(jobs, results):
        cases[case] = {"session": sid, "items": [dict(it, kind=k, change=c) for it, (k, c) in zip(items, kinds)]}
    return cases


def totals(cases):
    t = {f"{g}_{k}": 0 for g in ("passage", "nowhere") for k in KINDS + ("ERROR",)}
    for c in cases.values():
        for it in c["items"]:
            t[f"{'passage' if it.get('preview') else 'nowhere'}_{it['kind']}"] += 1
    return t


def line(name, t):
    parts = []
    for g in ("passage", "nowhere"):
        n = sum(t[f"{g}_{k}"] for k in KINDS + ("ERROR",))
        parts.append(f"{g} {n:>3}: " + " ".join(f"{k.lower()} {t[f'{g}_{k}']:>3}" for k in KINDS) + f" err {t[f'{g}_ERROR']}")
    return f"  {name:<12} " + "  |  ".join(parts)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("tags", nargs="+")
    ap.add_argument("--db", default=jp.DEFAULT_DB)
    ap.add_argument("--env", default=os.path.join(HERE, "judge.env"))
    ap.add_argument("--workers", type=int, default=4)
    args = ap.parse_args()

    cfg = judge.judge_config(args.env, judge.ev.DEFAULT_BASE)
    ask = judge.make_ask(cfg, timeout=600, system=SYSTEM_PROMPT, build=lambda it: it["message"], max_tokens=jp.MAX_TOKENS)
    conn = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    conn.text_factory = lambda b: b.decode("utf-8", "replace")

    for tag in args.tags:
        for name in br.resolve_rounds(tag, jp.OUT):
            path = os.path.join(jp.OUT, f"judgereview_{name}.json")
            if os.path.exists(path):
                with open(path) as f:
                    cases = json.load(f)["cases"]
            else:
                cases = review_round(name, conn, ask, args.workers)
                with open(path, "w") as f:
                    json.dump({"round": name, "judge": cfg["model"], "cases": cases}, f, ensure_ascii=False, indent=1)
            print(line(name, totals(cases)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
