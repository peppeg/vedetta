#!/usr/bin/env python3
"""Case-by-case comparison of two benchmark rounds (V25-R2.5). Diagnosis tool.

For every case present in both rounds, reads the two sessions through the API
and prints how much of the path was the same:
  queries   search queries (tree nodes) run in both rounds / distinct queries over the two rounds
  pages     pages that facts come from in both rounds / distinct pages over the two rounds
  facts     number of facts in A and B
  same      facts with the same page and verbatim snippet in both rounds / the larger fact count
  order     pairs of those shared facts that come in the same order in both rounds (V25-V2);
            IDs themselves are not compared: one extra fact early on shifts every later number
  judge     % SUPPORTED of A and B (from judge_TAG_<session>.txt, when present)

Usage:
  python3 scripts/compare_runs.py RUN_A RUN_B [--base-url http://localhost:3031]

RUN_A and RUN_B are round names as in scripts/out/runs_NAME.json.
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import bench_report as br  # noqa: E402
import eval as ev  # noqa: E402


def last_answer(session):
    msgs = [m for m in session.get("messages") or [] if m.get("role") == "assistant"]
    return msgs[-1] if msgs else {}


def norm_query(q):
    return " ".join((q or "").lower().split())


def norm_url(u):
    return (u or "").strip().rstrip("/")


def queries(msg):
    """Search queries actually run: the tree nodes below the root, or the planner sub-queries."""
    tree = msg.get("tree") or {}
    nodes = [n for n in tree.get("nodes") or [] if n.get("parent")]
    found = [n.get("query") for n in nodes] or [s.get("query") for s in msg.get("subqueries") or []]
    return {norm_query(q) for q in found if q}


def fact_pages(msg):
    return {norm_url(f.get("source_url")) for f in msg.get("facts") or [] if f.get("source_url")}


def fact_keys(msg):
    """(page, snippet) of every fact, in ID order."""
    def num(f):
        digits = (f.get("id") or "").lstrip("F")
        return int(digits) if digits.isdigit() else 0
    facts = sorted(msg.get("facts") or [], key=num)
    return [(norm_url(f.get("source_url")), f.get("verbatim_snippet")) for f in facts]


def order_kept(a, b):
    """Share of pairs of facts present in both rounds that keep their relative order."""
    common = set(a) & set(b)
    sa = [k for k in dict.fromkeys(a) if k in common]
    pos = {k: i for i, k in enumerate(dict.fromkeys(k for k in b if k in common))}
    pairs = [(x, y) for i, x in enumerate(sa) for y in sa[i + 1:]]
    if not pairs:
        return "-"
    return f"{100 * sum(pos[x] < pos[y] for x, y in pairs) / len(pairs):.0f}%"


def overlap(a, b):
    return f"{len(a & b)}/{len(a | b)}"


def judge_pct(out_dir, run, sid):
    path = os.path.join(out_dir, f"judge_{run}_{sid}.txt")
    if not os.path.exists(path):
        return None
    with open(path) as f:
        j = br.parse_judge(f.read())
    return br.pct(j["supported"], j["judged"]) if j else None


def summarize(msg):
    return {"queries": queries(msg), "pages": fact_pages(msg), "facts": len(msg.get("facts") or []),
            "keys": fact_keys(msg)}


def compare_row(case, a, b, ja, jb):
    vote = lambda v: "n/a" if v is None else f"{v:.0f}"  # noqa: E731
    most = max(a["facts"], b["facts"])
    same = f"{len(set(a['keys']) & set(b['keys']))}/{most}"
    return (f"{case:<32} queries {overlap(a['queries'], b['queries']):>5}   pages {overlap(a['pages'], b['pages']):>5}   "
            f"facts {a['facts']:>2} -> {b['facts']:<2}   same {same:>5}   order {order_kept(a['keys'], b['keys']):>4}   judge {vote(ja):>3} -> {vote(jb)}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("run_a")
    ap.add_argument("run_b")
    ap.add_argument("--base-url", default=ev.DEFAULT_BASE)
    ap.add_argument("--out", default=br.DEFAULT_OUT)
    args = ap.parse_args()

    runs = []
    for run in (args.run_a, args.run_b):
        path = os.path.join(args.out, f"runs_{run}.json")
        if not os.path.exists(path):
            sys.exit(f"missing {path}")
        with open(path) as f:
            runs.append(json.load(f))
    ra, rb = runs

    print(f"A = {args.run_a}   B = {args.run_b}   ({args.base_url})")
    for case in [c for c in ra if c in rb]:
        sa, sb = ra[case], rb[case]
        try:
            a = summarize(last_answer(ev.fetch_session(args.base_url, sa)))
            b = summarize(last_answer(ev.fetch_session(args.base_url, sb)))
        except Exception as e:  # one unreadable session must not stop the table
            print(f"{case:<32} cannot read sessions: {e}")
            continue
        print(compare_row(case, a, b, judge_pct(args.out, args.run_a, sa), judge_pct(args.out, args.run_b, sb)))
    only = sorted(set(ra) ^ set(rb))
    if only:
        print(f"cases in one round only: {', '.join(only)}")


if __name__ == "__main__":
    main()
