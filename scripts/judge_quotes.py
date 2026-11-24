#!/usr/bin/env python3
"""Vedetta - does the passage found for a sentence say what the sentence says?

Phase 6.5 (docs/SPEC_FASE6_CITAZIONI.md §9). The Deep a ciclo, with VEDETTA_DEEP_LOOP_QUOTES,
asks the model for the passage of a page that says each sentence, and Go finds it verbatim.
Finding it proves the text is on the page, not that it supports the sentence. Here every
sentence with state "found" is judged on the paragraph of the page that holds its passage
(not on the whole page), with the verdicts of judge_links.py:

  SUPPORTED / PARTIAL / ABSENT / CONTRADICTED

The paragraph is read from the round's cache (what the model saw): the passage's own words
(evidence) are looked for in the page and widened to the paragraphs they touch, up to PARA_CHARS. A
sentence with two passages gets both. If the page is not in the cache, the evidence alone.

Usage:
  python3 scripts/judge_quotes.py NAME --db DB --cache DIR --runs runs_ROUND.json [--dry-run]
      [--limit N] [--env scripts/judge.env] [--workers 4]
--dry-run counts calls and characters and spends nothing. Writes scripts/out/judgequotes_NAME.json.
Stdlib only.
"""
import argparse
import collections
import json
import os
import re
import sys
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import eval as ev  # noqa: E402
import judge  # noqa: E402
import judge_links as jl  # noqa: E402
import judge_pages as jp  # noqa: E402

OUT = os.path.join(HERE, "out")
PARA_CHARS = 1500   # the most of a page's paragraph shown with a passage
BATCH = 10          # sentences per judge call
MAX_TOKENS = 16384

SYSTEM_PROMPT = """You are a strict fact-checking judge.
You get a numbered list of SENTENCES from an answer. Under each one is the PASSAGE of a web page that the answer points to as the place where the sentence comes from.
For each sentence, decide using only its passage, with no outside knowledge:
- SUPPORTED: every factual detail of the sentence (numbers, dates, names, qualifiers) is stated in or directly implied by the passage.
- PARTIAL: the passage supports the main point, but the sentence adds details, numbers or qualifiers the passage does not contain.
- ABSENT: the passage does not say what the sentence says.
- CONTRADICTED: the passage says something incompatible with the sentence.
Text marked (context: ...) only helps to understand the sentence; do not judge it. The texts may be in different languages.
Answer with one line per sentence, exactly "n: VERDICT", and nothing else."""

VERDICTS = ("SUPPORTED", "PARTIAL", "ABSENT", "CONTRADICTED")


def _page_index(page, flat_at):
    """The index in page of the character at flat_at in re.sub(r"\\s+", " ", page)."""
    removed = 0
    for m in re.finditer(r"\s+", page):
        if m.start() - removed > flat_at:
            break
        if m.start() - removed == flat_at:
            return m.start()
        removed += len(m.group()) - 1
    return flat_at + removed


def _around(page, flat, at, length, cap):
    """The paragraphs of page that flat[at:at+length] touches, cut to cap characters around it."""
    needle = flat[at:at + length]
    first = _page_index(page, at)
    last = _page_index(page, at + length - 1)
    lo = page.rfind("\n\n", 0, first)
    lo = 0 if lo < 0 else lo + 2
    hi = page.find("\n\n", last + 1)
    hi = len(page) if hi < 0 else hi
    text = re.sub(r"\s+", " ", page[lo:hi]).strip()
    if len(text) > cap:
        mid = max(0, text.find(needle))
        start = max(0, min(mid - (cap - length) // 2, len(text) - cap))
        text = text[start:start + cap]
    return text


def paragraph(page, evidence):
    """The paragraphs of page that evidence touches (one, or more when it runs over a paragraph
    break), cut to PARA_CHARS around it; "" if it is not there. The parts of a quote with an
    ellipsis, which Go joins with " … ", are shown together when they are close, else each one
    with its own paragraph, joined the same way."""
    if not page or not evidence:
        return ""
    flat = re.sub(r"\s+", " ", page)
    key = re.sub(r"\s+", " ", evidence).strip()
    at = flat.find(key)
    if at >= 0:
        return _around(page, flat, at, len(key), PARA_CHARS)
    parts = [p.strip() for p in key.split(" … ") if p.strip()]
    if len(parts) > 1:
        spans, pos = [], 0
        for p in parts:
            i = flat.find(p, pos)
            if i < 0:
                break
            spans.append((i, len(p)))
            pos = i + len(p)
        else:
            whole = spans[-1][0] + spans[-1][1] - spans[0][0]
            if whole <= PARA_CHARS:
                return _around(page, flat, spans[0][0], whole, PARA_CHARS)
            cap = PARA_CHARS // len(spans)
            return " … ".join(_around(page, flat, i, n, cap) for i, n in spans)
    key = key[:80].rstrip()  # only its start is on the page as it is
    at = flat.find(key)
    return _around(page, flat, at, len(key), PARA_CHARS) if at >= 0 else ""


def units_of(turn, cache_dir):
    out = []
    items = ((turn["meta"].get("loop") or {}).get("quote_items")) or []
    for it in items:
        if it.get("state") != "found":
            continue
        passages = []
        for r in it.get("refs") or []:
            if not r.get("found"):
                continue
            page = jp.cached_page(cache_dir, r.get("url", "")) if cache_dir else None
            para = paragraph(page, r.get("evidence", "")) or r.get("evidence", "")
            if para:
                passages.append(para)
        if passages:
            out.append({"sid": turn["sid"], "sentence": it["sentence"], "passages": passages,
                        "numbers_out": it.get("numbers_out") or [], "chars": sum(len(p) for p in passages)})
    return out


def message(units):
    lines = []
    for i, u in enumerate(units, 1):
        body = "\n   ".join(f"PASSAGE: {p}" for p in u["passages"])
        lines.append(f"{i}. {u['sentence']}\n   {body}")
    return "SENTENCES:\n" + "\n".join(lines)


def judge_batch(ask, units):
    return jl.parse_verdicts(ask({"message": message(units)}), len(units))


def pct(n, d):
    return round(100 * n / d, 1) if d else None


def summarize(units):
    def row(us):
        c = collections.Counter(u.get("verdict", "ERROR") for u in us)
        judged = sum(c[v] for v in VERDICTS)
        return {**{v: c[v] for v in VERDICTS + ("ERROR",)}, "judged": judged, "pct_supported": pct(c["SUPPORTED"], judged)}
    return {"all": row(units),
            "numbers all inside the passage": row([u for u in units if not u["numbers_out"]]),
            "some number not in the passage": row([u for u in units if u["numbers_out"]])}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("name")
    ap.add_argument("--db", required=True)
    ap.add_argument("--cache", required=True)
    ap.add_argument("--runs", required=True)
    ap.add_argument("--limit", type=int, default=0, help="only this many sentences, spread over the round (a pilot)")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--env", default=os.path.join(HERE, "judge.env"))
    ap.add_argument("--workers", type=int, default=4)
    args = ap.parse_args()

    turns = jl.load_turns(args.db, args.runs)
    units = [u for t in turns for u in units_of(t, args.cache)]
    if args.limit:
        units = units[:: max(1, len(units) // args.limit)][:args.limit]
    batches = [units[i:i + BATCH] for i in range(0, len(units), BATCH)]
    chars = sum(len(message(b)) for b in batches)
    print(f"{args.name}: {len(turns)} answers, {len(units)} found sentences, {len(batches)} judge calls, "
          f"{chars} characters (~{chars // 4} tokens)", flush=True)
    if args.dry_run:
        return

    cfg = judge.judge_config(args.env, ev.DEFAULT_BASE)
    ask = judge.make_ask(cfg, timeout=600, system=SYSTEM_PROMPT, build=lambda it: it["message"], max_tokens=MAX_TOKENS)
    with ThreadPoolExecutor(max_workers=max(1, args.workers)) as pool:
        for b, verdicts in zip(batches, pool.map(lambda x: judge_batch(ask, x), batches)):
            for u, v in zip(b, verdicts):
                u["verdict"] = v
    summary = summarize(units)
    for k, r in summary.items():
        print(f"  {k:<32} judged {r['judged']:>4}  SUPPORTED {r['SUPPORTED']:>4} ({r['pct_supported']}%)  PARTIAL {r['PARTIAL']:>4}  "
              f"ABSENT {r['ABSENT']:>4}  CONTRADICTED {r['CONTRADICTED']:>3}  errors {r['ERROR']}")
    os.makedirs(OUT, exist_ok=True)
    path = os.path.join(OUT, f"judgequotes_{args.name}.json")
    with open(path, "w") as f:
        json.dump({"name": args.name, "judge": cfg.get("model"), "summary": summary, "units": units}, f, ensure_ascii=False, indent=1)
    print("written", path)


if __name__ == "__main__":
    main()
