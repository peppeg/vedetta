#!/usr/bin/env python3
"""Vedetta - the ruler: question understanding and answer completeness (V25-QB).

Measures what the faithfulness judge cannot see, against the hand-written case
sheets in scripts/expected.json (the engine never reads them):

  understanding (deterministic, QB.2), from the searches and facts of a session:
    joint       searches naming 2+ entities of the sheet
    courtesy    searches carrying conversation words ("grazie", "thanks", ...)
    unread      entities no fact mentions (no page read gave anything about them)
    official    distinct pages on an entity's official home that gave facts

  completeness (judge model, QB.3): for every expected content, one call that
  sees only the answer body and the content and says COVERED (concretely, in
  cited sentences), PARTIAL (only some items, or generic information), UNCITED
  (only uncited or declared-deduction sentences), DECLARED_MISSING (the answer
  says it did not find it) or ABSENT. The number of
  the ruler is COVERED / expected contents, summed over the cases.

Pages are counted through the facts they gave: the session does not record the
pages that were read and gave nothing (those are only in the server log).

Usage:
  python3 scripts/ruler.py TAG [TAG ...] [--db PATH] [--no-completeness] [--passes N]
    reads the rounds' sessions from the bench database and writes
    scripts/out/ruler_NAME.json per round, then prints a summary.
    --passes N judges the completeness N times on the same answers (calibration,
    spec D4); the first pass is the round's number.
Stdlib only.
"""
import argparse
import json
import os
import re
import sqlite3
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_EXPECTED = os.path.join(HERE, "expected.json")
DEFAULT_OUT = os.path.join(HERE, "out")
DEFAULT_DB = os.path.expanduser("~/vedetta-bench/data/vedetta.db")

# Conversation words that do not belong in a search (5 answer languages).
COURTESY = {
    "it": ["grazie", "per favore", "per piacere", "così riesco", "cosi riesco", "ti ringrazio", "gentilmente",
           "mi faresti", "potresti"],
    "en": ["thanks", "thank you", "please", "could you", "would you"],
    "de": ["danke", "bitte", "könntest du", "koenntest du"],
    "fr": ["merci", "s'il vous plaît", "s'il te plaît", "pourriez-vous"],
    "es": ["gracias", "por favor", "podrías", "podrias"],
}
COURTESY_RE = re.compile(
    r"(?<![\w])(?:" + "|".join(re.escape(w) for ws in COURTESY.values() for w in ws) + r")(?![\w])",
    re.IGNORECASE)


COMPLETENESS_VERDICTS = ("COVERED", "PARTIAL", "UNCITED", "DECLARED_MISSING", "ABSENT")

COMPLETENESS_PROMPT = """You check whether an ANSWER covers a TOPIC. Do not judge whether the answer is correct: only whether it gives the information the topic asks for.
Read the whole answer, then reply with exactly one word:
- COVERED: the answer gives the information the topic asks for, concretely, in sentences that carry citation markers such as [3] or [S2]. When the topic says "each" or names several items, the answer must give it for all or nearly all of them.
- PARTIAL: the answer gives only part of it: for some of the items only, or only generic or indirect information instead of what the topic asks.
- UNCITED: the answer gives the information, but only in sentences without citation markers, or only in sentences labelled as the writer's own reasoning ("Deduzione, non scritta nelle fonti", "Inference, not stated in the sources" and the same label in other languages), even if they carry markers.
- DECLARED_MISSING: the answer says that the main information on this topic was not found or is not available. Choose this even if the answer adds indirect hints or side details.
- ABSENT: the answer does not deal with the topic. Naming the topic's words without giving any information about it counts as ABSENT.
The answer and the topic may be in different languages."""


def completeness_message(item):
    return f"TOPIC: {item['content']}\n\nANSWER:\n{item['body']}"


def parse_completeness(text):
    t = (text or "").upper()
    for v in ("DECLARED_MISSING", "PARTIAL", "UNCITED", "COVERED", "ABSENT"):
        if re.search(r"\b" + v + r"\b", t):
            return v
    return "ERROR"


def completeness(turn, sheet, ask, workers=4):
    """One verdict per expected content of the case (QB.3)."""
    import eval as ev  # noqa: E402  (same directory)
    from concurrent.futures import ThreadPoolExecutor
    body = ev.answer_body(turn.get("content") or "")
    items = [{"content": c, "body": body} for c in sheet.get("contents") or []]
    if not items:
        return []
    if not body.strip():
        return [{"content": it["content"], "verdict": "ABSENT", "raw": "empty answer"} for it in items]
    with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        answers = list(pool.map(ask, items))
    return [{"content": it["content"], "verdict": parse_completeness(a), "raw": a[:200]} for it, a in zip(items, answers)]


def completeness_totals(per_case):
    t = {v: 0 for v in COMPLETENESS_VERDICTS + ("ERROR",)}
    for items in per_case.values():
        for it in items:
            t[it["verdict"]] += 1
    t["expected"] = sum(len(items) for items in per_case.values())
    t["pct"] = round(100 * t["COVERED"] / t["expected"], 1) if t["expected"] else None
    return t


def completeness_line(t):
    pct = "n/a" if t["pct"] is None else f"{t['pct']:.1f}%"
    return (f"completeness: {pct} (COVERED {t['COVERED']}/{t['expected']}, partial {t['PARTIAL']}, uncited {t['UNCITED']}, "
            f"declared missing {t['DECLARED_MISSING']}, absent {t['ABSENT']}"
            + (f", judge errors {t['ERROR']}" if t["ERROR"] else "") + ")")


def load_expected(path=DEFAULT_EXPECTED):
    with open(path) as f:
        return json.load(f).get("cases", {})


def _as_list(v):
    if isinstance(v, str):
        try:
            v = json.loads(v)
        except ValueError:
            return []
    return v if isinstance(v, list) else []


def names_of(entity):
    return [n for n in [entity.get("name", "")] + list(entity.get("aliases") or []) if n]


def mentions(text, entity):
    """Whether text names the entity (name or alias, whole words, any case)."""
    low = (text or "").lower()
    for n in names_of(entity):
        if re.search(r"(?<![\w])" + re.escape(n.lower()) + r"(?![\w])", low):
            return True
    return False


def on_home(url, entity):
    """Whether url is on one of the entity's official homes (domain or URL prefix)."""
    m = re.match(r"https?://(?:www\.)?([^?#]*)", (url or "").strip(), re.IGNORECASE)
    if not m:
        return False
    rest = m.group(1).lower()
    host = rest.split("/", 1)[0]
    for h in entity.get("home") or []:
        h = h.lower().strip("/")
        if "/" in h:
            if rest == h or rest.startswith(h + "/"):
                return True
        elif host == h or host.endswith("." + h):
            return True
    return False


def understanding(turn, sheet):
    """The four understanding counts of one session (QB.2)."""
    entities = sheet.get("entities") or []
    queries = [s.get("query", "") if isinstance(s, dict) else str(s) for s in _as_list(turn.get("subqueries"))]
    facts = [f for f in _as_list(turn.get("facts")) if isinstance(f, dict)]

    joint = [q for q in queries if sum(1 for e in entities if mentions(q, e)) >= 2]
    courtesy = [q for q in queries if COURTESY_RE.search(q)]

    unread, official = [], {}
    for e in entities:
        text_hit = any(
            mentions(" ".join(str(f.get(k) or "") for k in ("claim", "evidence", "passage", "source_title", "source_url")), e)
            for f in facts)
        pages = sorted({f.get("source_url", "") for f in facts if on_home(f.get("source_url", ""), e)})
        if not text_hit and not pages:
            unread.append(e["name"])
        if e.get("home"):
            official[e["name"]] = len(pages)
    return {"queries": len(queries), "joint": joint, "courtesy": courtesy,
            "entities": len(entities), "unread": unread, "official": official}


def understanding_line(u):
    official = ", ".join(f"{k} {v}" for k, v in u["official"].items()) or "n/a"
    return (f"understanding: joint {len(u['joint'])}/{u['queries']}  courtesy {len(u['courtesy'])}/{u['queries']}  "
            f"entities without facts {len(u['unread'])}/{u['entities']}"
            + (f" ({', '.join(u['unread'])})" if u["unread"] else "")
            + f"  official pages with facts: {official}")


def totals(per_case):
    """Sum of the understanding counts over the cases of a round."""
    t = {"queries": 0, "joint": 0, "courtesy": 0, "entities": 0, "unread": 0, "official": 0}
    for u in per_case.values():
        t["queries"] += u["queries"]
        t["joint"] += len(u["joint"])
        t["courtesy"] += len(u["courtesy"])
        t["entities"] += u["entities"]
        t["unread"] += len(u["unread"])
        t["official"] += sum(u["official"].values())
    return t


def totals_line(t):
    return (f"understanding: joint {t['joint']}/{t['queries']}, courtesy {t['courtesy']}/{t['queries']}, "
            f"entities without facts {t['unread']}/{t['entities']}, official pages with facts {t['official']}")


# ---------------------------------------------------------------- rounds

def turn_from_db(conn, session_id):
    row = conn.execute(
        "select content, sources_json, facts_json, subqueries_json, meta_json from messages "
        "where session_id = ? and role = 'assistant' order by created_at limit 1", (session_id,)).fetchone()
    if not row:
        return None
    content, sources, facts, subqueries, meta = row
    try:
        meta = json.loads(meta) if meta else None
    except ValueError:
        meta = None
    return {"content": content or "", "sources": sources, "facts": facts, "subqueries": subqueries, "meta": meta}


def measure_round(name, out_dir, conn, expected, ask=None, passes=1, workers=4):
    """Understanding of every case; with ask, also completeness (passes times)."""
    with open(os.path.join(out_dir, f"runs_{name}.json")) as f:
        runs = json.load(f)
    per_case, problems, turns = {}, [], {}
    for case, sid in runs.items():
        sheet = expected.get(case)
        if sheet is None:
            problems.append(f"no sheet for {case}")
            continue
        turn = turn_from_db(conn, sid)
        if turn is None:
            problems.append(f"session of {case} not in the database")
            continue
        per_case[case] = understanding(turn, sheet)
        turns[case] = (turn, sheet)
    rec = {"round": name, "understanding": per_case, "problems": problems}
    if ask is not None:
        rec["completeness_passes"] = [
            {case: completeness(turn, sheet, ask, workers) for case, (turn, sheet) in turns.items()}
            for _ in range(max(1, passes))]
        rec["completeness"] = rec["completeness_passes"][0]
    return rec


def main():
    sys.path.insert(0, HERE)
    import bench_report as br  # noqa: E402

    ap = argparse.ArgumentParser(description="Vedetta ruler: understanding and completeness (V25-QB)")
    ap.add_argument("tags", nargs="+", help="round tags, as for bench_report.py")
    ap.add_argument("--db", default=DEFAULT_DB, help="database holding the rounds' sessions")
    ap.add_argument("--out", default=DEFAULT_OUT)
    ap.add_argument("--expected", default=DEFAULT_EXPECTED)
    ap.add_argument("--no-completeness", action="store_true", help="understanding only, no judge calls")
    ap.add_argument("--passes", type=int, default=1, help="completeness passes on the same answers (calibration)")
    ap.add_argument("--env", default=os.path.join(HERE, "judge.env"), help="judge env file, as for judge.py")
    ap.add_argument("--workers", type=int, default=4)
    args = ap.parse_args()

    expected = load_expected(args.expected)
    ask = None
    if not args.no_completeness:
        import judge  # noqa: E402
        cfg = judge.judge_config(args.env, judge.ev.DEFAULT_BASE)
        ask = judge.make_ask(cfg, system=COMPLETENESS_PROMPT, build=completeness_message)
    conn = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    conn.text_factory = lambda b: b.decode("utf-8", "replace")
    for tag in args.tags:
        names = br.resolve_rounds(tag, args.out)
        if not names:
            sys.exit(f"no runs file for tag {tag!r} in {args.out}")
        for name in names:
            rec = measure_round(name, args.out, conn, expected, ask, args.passes, args.workers)
            with open(os.path.join(args.out, f"ruler_{name}.json"), "w") as f:
                json.dump(rec, f, ensure_ascii=False, indent=1)
            print(f"{name}: {totals_line(totals(rec['understanding']))}")
            for i, p in enumerate(rec.get("completeness_passes") or [], 1):
                print(f"  pass {i}: {completeness_line(completeness_totals(p))}")
            for p in rec["problems"]:
                print(f"  problem: {p}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
