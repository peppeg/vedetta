#!/usr/bin/env python3
"""Vedetta - every claim judged on the pages actually read (PIANO_DEEP_A_CICLO §9).

The same method for the pipeline Deep and the Deep a ciclo, so the two can be
compared: for every answer of a benchmark round the judge sees the full text of
the pages the session read (from the round's cache: what the model got, up to the conversation limit) and the
search previews it was shown, and gives one verdict per sentence of the answer:

  SUPPORTED / PARTIAL / UNSUPPORTED  a factual claim and how the sources back it
  ABSENCE     the sentence only says that something was not found or does not
              exist, or that the assistant will not do something: no passage can
              support it, so it is counted apart, never as unsupported
  OTHER       not a claim (greeting, offer of help, a question to the user)

Sentences opened by Go's deduction label are not sent (declared deductions).
% SUPPORTED = SUPPORTED / (SUPPORTED + PARTIAL + UNSUPPORTED).

Pages read:
  Deep a ciclo  meta_json.loop.calls_detail, "read" calls with result "ok"
  pipeline      the round's server log, "[reader] session=... url=..." lines
Texts come from the round's cache (~/vedetta-bench/data/cache-live-ROUND/page),
looked up with the same key as internal/cache/keys.go (PageKey, keyVersion v1).
The passages of result pages that the sentinel opened and showed the model
(VEDETTA_DEEP_LOOP_REVIEW_PREVIEWS, meta_json.loop.review_items/review_confirmed)
are sources too.

Usage:
  python3 scripts/judge_pages.py TAG [TAG ...] [--baseline TAG ...]
Writes scripts/out/judgepages_ROUND.json and prints the totals. Stdlib only.
"""
import argparse
import hashlib
import json
import os
import re
import sqlite3
import sys
import urllib.parse
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import bench_report as br  # noqa: E402
import eval as ev  # noqa: E402
import judge  # noqa: E402

BENCH = os.environ.get("VEDETTA_BENCH_DIR", os.path.expanduser("~/vedetta-bench"))
DEFAULT_DB = os.path.join(BENCH, "data", "vedetta.db")
OUT = os.path.join(HERE, "out")
KEY_VERSION = "v1"  # internal/cache/keys.go keyVersion

PAGE_CHARS = 40000    # per page shown to the judge, pipeline only: the Deep a ciclo shows what the model got
TOTAL_CHARS = 600000  # all pages and previews of one answer: the Deep a ciclo's conversation limit
LOOP_PART_CHARS = 30000  # deeploop.go before 07/11/2026: long pages were read in parts of about this size
BATCH = 20            # sentences per judge call
MAX_TOKENS = 16384    # reasoning plus one verdict line per sentence (1024 left the answers empty)
MIN_SENTENCE = 25

VERDICTS = ("SUPPORTED", "PARTIAL", "UNSUPPORTED", "ABSENCE", "OTHER")

SYSTEM_PROMPT = """You are a strict fact-checking judge.
You get SOURCES (web pages and search previews that an assistant read) and a numbered list of SENTENCES from the assistant's answer.
For each sentence, decide using only the sources, no outside knowledge:
- SUPPORTED: every factual detail (numbers, dates, names, qualifiers) is stated in or directly implied by the sources.
- PARTIAL: the sources support the main point, but the sentence adds details, numbers or qualifiers they do not contain.
- UNSUPPORTED: the sources do not support the sentence, or contradict it.
- ABSENCE: the sentence only says that something was not found, does not exist, or that the assistant will not or cannot do something.
- OTHER: not a factual claim (a greeting, an offer of help, a question to the user, a heading).
The texts may be in different languages. Answer with one line per sentence, exactly "n: VERDICT", and nothing else."""

LINE_RE = re.compile(r"^\s*\**\s*(\d+)\s*\**\s*[:.)\-]\s*\**\s*(SUPPORTED|PARTIAL|UNSUPPORTED|ABSENCE|OTHER)\b", re.I | re.M)
READER_RE = re.compile(r"\[reader\] session=(\S+) url=(\S+) (?:served_by=\S+ )?chars=(\d+)")


def page_key(url):
    """internal/cache/keys.go PageKey: lowercase scheme and host, no fragment."""
    u = urllib.parse.urlsplit(url.strip())
    if not u.scheme:
        return f"{KEY_VERSION}|page|{url.strip()}"
    u = u._replace(scheme=u.scheme.lower(), netloc=u.netloc.lower(), fragment="")
    return f"{KEY_VERSION}|page|{urllib.parse.urlunsplit(u)}"


def cached_page(cache_dir, url):
    h = hashlib.sha256(page_key(url).encode()).hexdigest()
    path = os.path.join(cache_dir, "page", h[:2], h + ".json")
    try:
        with open(path) as f:
            e = json.load(f)
    except (OSError, ValueError):
        return None
    doc = e.get("document") or {}
    return doc.get("content") or None


def pages_read(turn, sid, server_log):
    """URLs the session read, in order, without repeats."""
    meta = turn.get("meta") or {}
    urls = []
    if meta.get("loop"):
        urls = [c["url"] for c in meta["loop"].get("calls_detail") or [] if c.get("tool") == "read" and c.get("result") == "ok"]
    else:
        urls = [m.group(2) for m in READER_RE.finditer(server_log) if m.group(1) == sid and int(m.group(3)) > 0]
    seen, out = set(), []
    for u in urls:
        if u not in seen:
            seen.add(u)
            out.append(u)
    return out


def split_parts(text, size=LOOP_PART_CHARS):
    """splitParts of deeploop.go before 07/11/2026: parts of about size bytes, cut at
    a line end in the last tenth when there is one, never inside a UTF-8 character."""
    b = text.encode()
    parts = []
    while len(b) > size:
        cut = size
        nl = b.rfind(b"\n", size - size // 10, size)
        if nl >= 0:
            cut = nl + 1
        while cut > 0 and (b[cut] & 0xC0) == 0x80:
            cut -= 1
        parts.append(b[:cut])
        b = b[cut:]
    if b.strip() or not parts:
        parts.append(b)
    return [p.decode("utf-8", "replace") for p in parts]


def seen_by_model(calls, url, text):
    """The part of a page the Deep a ciclo gave the model. Before 07/11/2026 reads
    carry a part number and the model got only the parts it asked for; since then
    it gets the whole page (a page too long for the conversation is cut, and the
    session does not record where: rare, counted in loop.pages_partial)."""
    reads = [c for c in calls if c.get("tool") == "read" and c.get("url") == url and c.get("result") == "ok"]
    if any("part" in c for c in reads):
        return "".join(split_parts(text)[:max(c.get("part") or 1 for c in reads)])
    return text


def sources_text(turn, sid, cache_dir, server_log):
    """The sources block shown to the judge and how many pages it holds."""
    parts, total, used = [], 0, 0
    loop = (turn.get("meta") or {}).get("loop") or {}
    calls = loop.get("calls_detail") or []
    for i, url in enumerate(pages_read(turn, sid, server_log), 1):
        text = cached_page(cache_dir, url)
        if not text:
            continue
        text = seen_by_model(calls, url, text) if loop else text[:PAGE_CHARS]
        if total + len(text) > TOTAL_CHARS:
            break
        parts.append(f"[PAGE {i}] {url}\n{text}")
        total += len(text)
        used += 1
    passages = []
    for it in (loop.get("review_items") or []) + (loop.get("review_confirmed") or []):
        p = f"- {it['url']}: {it['passage']}" if it.get("passage") and it.get("url") else ""
        if p and p not in passages and total + len(p) <= TOTAL_CHARS:
            passages.append(p)
            total += len(p)
    if passages:
        parts.append("[PASSAGES VEDETTA OPENED]\n" + "\n".join(passages))
    previews = []
    for s in ev._as_list(turn.get("sources")):
        if isinstance(s, dict) and s.get("url"):
            p = f"- {s.get('title', '')} ({s['url']}): {s.get('content', '')}"
            if total + len(p) > TOTAL_CHARS:
                break
            previews.append(p)
            total += len(p)
    if previews:
        parts.append("[SEARCH PREVIEWS]\n" + "\n".join(previews))
    return "\n\n".join(parts), used


def answer_sentences(turn):
    body = ev.answer_body(turn.get("content") or "")
    out, declared = [], 0
    for s in judge.split_sentences(body):
        text = judge.clean_sentence(s)
        if judge.DECLARED_LABEL.match(text):
            declared += 1
        elif len(text) >= MIN_SENTENCE:
            out.append(text)
    return out, declared


def judge_answer(ask, sources, sentences):
    verdicts = {}
    for start in range(0, len(sentences), BATCH):
        chunk = sentences[start:start + BATCH]
        msg = "SOURCES:\n" + sources + "\n\nSENTENCES:\n" + "\n".join(f"{start + i + 1}. {s}" for i, s in enumerate(chunk))
        raw = ask({"message": msg})
        for m in LINE_RE.finditer(raw or ""):
            verdicts[int(m.group(1))] = m.group(2).upper()
    return [verdicts.get(i + 1, "ERROR") for i in range(len(sentences))]


def load_turn(conn, sid):
    row = conn.execute("select content, meta_json, sources_json from messages where session_id=? and role='assistant' "
                       "order by created_at limit 1", (sid,)).fetchone()
    if not row:
        return None
    return {"content": row[0], "meta": json.loads(row[1] or "{}"), "sources": json.loads(row[2] or "[]")}


def round_jobs(name, conn):
    with open(os.path.join(OUT, f"runs_{name}.json")) as f:
        runs = json.load(f)
    log_path = os.path.join(OUT, f"server_{name}.log")
    server_log = open(log_path, errors="replace").read() if os.path.exists(log_path) else ""
    cache_dir = os.path.join(BENCH, "data", f"cache-live-{name}")
    jobs = []
    for case, sid in runs.items():
        turn = load_turn(conn, sid)
        if turn is None:
            continue
        sources, used = sources_text(turn, sid, cache_dir, server_log)
        sentences, declared = answer_sentences(turn)
        jobs.append((case, sid, sources, used, sentences, declared))
    return jobs


def round_chars(name, conn):
    """Characters a judging of the round sends: the sources go again with every batch."""
    n = 0
    for _, _, sources, _, sentences, _ in round_jobs(name, conn):
        for start in range(0, len(sentences), BATCH):
            n += len(SYSTEM_PROMPT) + len(sources) + sum(len(s) + 6 for s in sentences[start:start + BATCH])
    return n


def judge_round(name, conn, ask, workers):
    jobs = round_jobs(name, conn)
    with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        results = list(pool.map(lambda j: judge_answer(ask, j[2], j[4]), jobs))
    cases = {}
    for (case, sid, _, used, sentences, declared), verdicts in zip(jobs, results):
        counts = {v: verdicts.count(v) for v in VERDICTS + ("ERROR",)}
        counts["declared"] = declared
        counts["pages"] = used
        cases[case] = {"session": sid, "counts": counts,
                       "items": [{"sentence": s, "verdict": v} for s, v in zip(sentences, verdicts)]}
    return cases


def totals(cases):
    t = {v: 0 for v in VERDICTS + ("ERROR", "declared", "pages")}
    for c in cases.values():
        for k in t:
            t[k] += c["counts"].get(k, 0)
    judged = t["SUPPORTED"] + t["PARTIAL"] + t["UNSUPPORTED"]
    t["judged"] = judged
    t["pct"] = round(100 * t["SUPPORTED"] / judged, 1) if judged else None
    return t


def line(name, t, n):
    return (f"  {name:<14} judged {t['judged']:>4}  SUPPORTED {t['SUPPORTED']:>4}  PARTIAL {t['PARTIAL']:>3}  "
            f"UNSUPPORTED {t['UNSUPPORTED']:>3}  % {t['pct']}  | absence {t['ABSENCE']}  other {t['OTHER']}  "
            f"errors {t['ERROR']}  declared {t['declared']}  pages/case {t['pages'] / max(1, n):.1f}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("tags", nargs="+")
    ap.add_argument("--baseline", nargs="+", default=[])
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--env", default=os.path.join(HERE, "judge.env"))
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--estimate", action="store_true", help="count the characters a judging would send, call no judge")
    args = ap.parse_args()

    if args.estimate:
        conn = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
        conn.text_factory = lambda b: b.decode("utf-8", "replace")
        total = 0
        for tag in args.tags + args.baseline:
            for name in br.resolve_rounds(tag, OUT):
                done = os.path.exists(os.path.join(OUT, f"judgepages_{name}.json"))
                n = 0 if done else round_chars(name, conn)
                total += n
                print(f"  {name:<14} {n / 1e6:6.2f} million characters" + ("  (already judged, reused)" if done else ""))
        print(f"total {total / 1e6:.2f} million characters")
        return 0

    cfg = judge.judge_config(args.env, judge.ev.DEFAULT_BASE)
    ask = judge.make_ask(cfg, timeout=600, system=SYSTEM_PROMPT, build=lambda it: it["message"], max_tokens=MAX_TOKENS)
    conn = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    conn.text_factory = lambda b: b.decode("utf-8", "replace")

    groups = {}
    for label, tags in (("rounds", args.tags), ("baseline", args.baseline)):
        rounds = []
        for tag in tags:
            for name in br.resolve_rounds(tag, OUT):
                path = os.path.join(OUT, f"judgepages_{name}.json")
                if os.path.exists(path):
                    with open(path) as f:
                        cases = json.load(f)["cases"]
                else:
                    cases = judge_round(name, conn, ask, args.workers)
                    with open(path, "w") as f:
                        json.dump({"round": name, "judge": cfg["model"], "cases": cases}, f, ensure_ascii=False, indent=1)
                rounds.append((name, cases))
        groups[label] = rounds

    pooled = {}
    for label, rounds in groups.items():
        if not rounds:
            continue
        print(f"{label}:")
        allc = {}
        for name, cases in rounds:
            print(line(name, totals(cases), len(cases)))
            allc.update({f"{name}/{k}": v for k, v in cases.items()})
        pooled[label] = totals(allc)
        print(f"  pooled % SUPPORTED on pages read: {pooled[label]['pct']}")
    if "baseline" in pooled and pooled["rounds"]["pct"] is not None and pooled["baseline"]["pct"] is not None:
        print(f"difference: {pooled['rounds']['pct'] - pooled['baseline']['pct']:+.1f} points")
    return 0


if __name__ == "__main__":
    sys.exit(main())
