#!/usr/bin/env python3
"""Vedetta - the PARTIAL and UNSUPPORTED sentences split by kind (PIANO_DEEP_A_CICLO §13, step 1).

judge_pages.py puts in PARTIAL both a sentence that adds the model's own
reasoning (advice, an inference, a sum of numbers from the sources) and one
that adds a detail the sources do not contain (a name, a date, a figure). The
first is what an answer is for, the second is the error Vedetta must catch.

This script takes the verdicts already in scripts/out/judgepages_ROUND.json,
sends again only the PARTIAL and UNSUPPORTED sentences with the same sources
(pages read + search previews, rebuilt as judge_pages.py does) and asks one of:

  REASONING  what the sentence adds beyond the sources is advice, a
             recommendation, an inference, a generalisation or a calculation
             from figures in the sources: no new specific fact
  INVENTED   it adds at least one specific fact the sources do not contain
             (name, date, number, price, version, place, rule, quote)

SUPPORTED counts are not touched, so the first-pass percentages stay as they
are. Two extra figures per round:
  % SUPPORTED+REASONING  = (SUPPORTED + reasoning) / judged
  % INVENTED             = invented / judged

Usage:
  python3 scripts/judge_partial.py TAG [TAG ...]
Writes scripts/out/judgepartial_ROUND.json and prints the totals. Stdlib only.
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

KINDS = ("REASONING", "INVENTED")
SPLIT = ("PARTIAL", "UNSUPPORTED")

SYSTEM_PROMPT = """You are a strict fact-checking judge.
You get SOURCES (web pages and search previews that an assistant read) and a numbered list of SENTENCES from the assistant's answer.
A first check found that each sentence is not fully backed by the sources. For each sentence, decide what it adds beyond the sources, using only the sources, no outside knowledge:
- REASONING: the addition is advice, a recommendation, an inference, a generalisation, or a calculation made from figures that are in the sources. It states no new specific fact.
- INVENTED: the sentence states at least one specific fact that the sources do not contain: a name, a date, a number, a price, a version, a place, a rule or requirement, a quotation.
The texts may be in different languages. Answer with one line per sentence, exactly "n: KIND | the added detail in a few words", and nothing else."""

LINE_RE = re.compile(r"^\s*\**\s*(\d+)\s*\**\s*[:.)\-]\s*\**\s*(REASONING|INVENTED)\b\**\s*(?:[|:\-]\s*(.*))?$", re.I | re.M)


def classify(ask, sources, sentences):
    out = {}
    for start in range(0, len(sentences), jp.BATCH):
        chunk = sentences[start:start + jp.BATCH]
        msg = "SOURCES:\n" + sources + "\n\nSENTENCES:\n" + "\n".join(f"{start + i + 1}. {s}" for i, s in enumerate(chunk))
        for m in LINE_RE.finditer(ask({"message": msg}) or ""):
            out[int(m.group(1))] = (m.group(2).upper(), (m.group(3) or "").strip())
    return [out.get(i + 1, ("ERROR", "")) for i in range(len(sentences))]


def split_round(name, conn, ask, workers):
    with open(os.path.join(jp.OUT, f"judgepages_{name}.json")) as f:
        cases = json.load(f)["cases"]
    log_path = os.path.join(jp.OUT, f"server_{name}.log")
    server_log = open(log_path, errors="replace").read() if os.path.exists(log_path) else ""
    cache_dir = os.path.join(jp.BENCH, "data", f"cache-live-{name}")
    jobs = []
    for case, c in cases.items():
        idx = [i for i, it in enumerate(c["items"]) if it["verdict"] in SPLIT]
        if not idx:
            continue
        turn = jp.load_turn(conn, c["session"])
        if turn is None:
            continue
        sources, _ = jp.sources_text(turn, c["session"], cache_dir, server_log)
        jobs.append((case, idx, sources, [c["items"][i]["sentence"] for i in idx]))
    with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        results = list(pool.map(lambda j: classify(ask, j[2], j[3]), jobs))
    for (case, idx, _, _), kinds in zip(jobs, results):
        for i, (kind, detail) in zip(idx, kinds):
            cases[case]["items"][i]["kind"] = kind
            cases[case]["items"][i]["detail"] = detail
    return cases


def totals(cases):
    t = {f"{v}_{k}": 0 for v in SPLIT for k in KINDS + ("ERROR",)}
    t.update({"SUPPORTED": 0, "judged": 0})
    for c in cases.values():
        for it in c["items"]:
            v = it["verdict"]
            if v == "SUPPORTED":
                t["SUPPORTED"] += 1
            elif v in SPLIT:
                t[f"{v}_{it.get('kind', 'ERROR')}"] += 1
            else:
                continue
            t["judged"] += 1
    n = max(1, t["judged"])
    reasoning = t["PARTIAL_REASONING"] + t["UNSUPPORTED_REASONING"]
    invented = t["PARTIAL_INVENTED"] + t["UNSUPPORTED_INVENTED"]
    t["pct"] = round(100 * t["SUPPORTED"] / n, 1)
    t["pct_with_reasoning"] = round(100 * (t["SUPPORTED"] + reasoning) / n, 1)
    t["pct_invented"] = round(100 * invented / n, 1)
    return t


def line(name, t):
    errors = sum(t[f"{v}_ERROR"] for v in SPLIT)
    return (f"  {name:<12} judged {t['judged']:>4}  % {t['pct']:>5}  "
            f"PARTIAL reas {t['PARTIAL_REASONING']:>3} inv {t['PARTIAL_INVENTED']:>3}  "
            f"UNSUP reas {t['UNSUPPORTED_REASONING']:>3} inv {t['UNSUPPORTED_INVENTED']:>3}  "
            f"% +reasoning {t['pct_with_reasoning']:>5}  % invented {t['pct_invented']:>5}  errors {errors}")


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
            path = os.path.join(jp.OUT, f"judgepartial_{name}.json")
            if os.path.exists(path):
                with open(path) as f:
                    cases = json.load(f)["cases"]
            else:
                cases = split_round(name, conn, ask, args.workers)
                with open(path, "w") as f:
                    json.dump({"round": name, "judge": cfg["model"], "cases": cases}, f, ensure_ascii=False, indent=1)
            print(line(name, totals(cases)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
