#!/usr/bin/env python3
"""Vedetta - does the page a link points to say what the sentence says?

No other ruler asks this (audit of 05/11/2026, docs/V25_LOG_VALUTAZIONI.md):
judge_pages judges a sentence on everything the model saw, whatever it links, and
links_in_sources only checks that the address was among the results. Here every
sentence of a Deep a ciclo answer that carries a link is judged on that page alone:

  SUPPORTED     every detail of the sentence is on the page
  PARTIAL       the main point is on the page, some details are not
  ABSENT        the page does not say it
  CONTRADICTED  the page says something incompatible
  POINTER       nothing to check: the sentence only names the page or sends to it

A link whose words are its own address ("Ideale sul N100. github.com/x/y") gives the
address of a project, it does not cite a source: it counts as POINTER and is not sent.

Pages the model read come from the round's cache (what it read then): they are the
control, and say how strict the ruler is. So does any other page the round opened.
All others are opened now with Vedetta's
readers, http then Jina as the home chain, and kept in data/link-pages/, so a second
run costs no fetching. A page may have changed since the answer was written. A page
that does not open is counted apart, never as unsupported.

Usage:
  python3 scripts/judge_links.py NAME --db DB --cache DIR [--runs FILE --where v=senza]
      [--fetch-only] [--dry-run] [--env scripts/judge.env] [--workers 4]
--runs also takes the runs file of a bench round (scripts/out/runs_ROUND.json).
Without --runs: all the Deep a ciclo answers of DB. --dry-run only counts what the
judge would receive. Writes scripts/out/judgelinks_NAME.json. Stdlib only.
"""
import argparse
import collections
import hashlib
import json
import os
import re
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import eval as ev  # noqa: E402
import judge  # noqa: E402
import judge_pages as jp  # noqa: E402

REPO = os.path.dirname(HERE)
OUT = os.path.join(HERE, "out")
STORE = os.path.join(REPO, "data", "link-pages")
# Of a page shown to the judge: the whole page, up to what judge_pages shows of one answer
# (Piano dopo audit 5.2). Until 13/11/2026 it was the first 40,000 characters, and up to 5% of
# the verdicts could be ABSENT only because the passage was further down: a judging made
# then is not comparable with one made now.
PAGE_CHARS = jp.TOTAL_CHARS
OLD_PAGE_CHARS = 40000   # what a saved judging without "page_chars" showed
BATCH = 20               # sentences per judge call
MAX_TOKENS = 16384       # reasoning plus one verdict line per sentence
MIN_CLAIM = 15           # below this a piece with links cites the sentence before it
MIN_PAGE = 200           # below this a page did not really open

VERDICTS = ("SUPPORTED", "PARTIAL", "ABSENT", "CONTRADICTED", "POINTER")
CLASSES = ("read", "preview", "vedetta", "other")

SYSTEM_PROMPT = """You are a strict fact-checking judge.
You get ONE web page and a numbered list of SENTENCES from an answer that links this page as a source.
For each sentence, decide using only this page, with no outside knowledge:
- SUPPORTED: every factual detail of the sentence (numbers, dates, names, qualifiers) is stated in or directly implied by this page.
- PARTIAL: this page supports the main point, but the sentence adds details, numbers or qualifiers the page does not contain.
- ABSENT: this page does not say what the sentence says.
- CONTRADICTED: this page says something incompatible with the sentence.
- POINTER: the sentence states nothing to check: it only names this page, gives its address, recommends it as further reading, or introduces what follows (a line ending in a colon).
A sentence may link other pages too: judge it on this page alone. Text marked (context: ...) only helps to understand the sentence; do not judge it.
The texts may be in different languages. Answer with one line per sentence, exactly "n: VERDICT", and nothing else."""

LINE_RE = re.compile(r"^\s*\**\s*(\d+)\s*\**\s*[:.)\-]\s*\**\s*(SUPPORTED|PARTIAL|ABSENT|CONTRADICTED|POINTER)\b", re.I | re.M)
# As deeploop.go answerLinkRe: what the answer links, not addresses written in code.
LINK_RE = re.compile(r"\]\((https?://[^)\s]+)\)|\[(https?://[^\]\s]+)\]")
LABELLED_LINK_RE = re.compile(r"\[([^\]]*)\]\((https?://[^)\s]+)\)")
ADDRESS_WORDS_RE = re.compile(r"^(?:https?://\S+|(?:www\.)?[\w-]+(?:\.[\w-]+)+/\S*)$")
LINK_WORDS_RE = re.compile(r"\[[^\]]*\]\([^)]*\)|\[https?://[^\]]*\]")
LIST_MARK = re.compile(r"^(?:[-*+]|\d+[.)])\s+")
RULE = re.compile(r"[-*_|:\s]{3,}")
NOTES_MARK = "<!-- vedetta:notes -->"


def links_in(text):
    return [m.group(1) or m.group(2) for m in LINK_RE.finditer(text)]


def address_links(part):
    """The links of a piece whose words are their own address, with a path."""
    return {u for words, u in LABELLED_LINK_RE.findall(part) if ADDRESS_WORDS_RE.match(words.strip())}


def answer_body(content):
    return ev.answer_body((content or "").split(NOTES_MARK)[0])


def words_outside_links(part):
    """What a piece of a sentence says besides the words of its links."""
    return re.sub(r"[^\w]+", " ", LINK_WORDS_RE.sub(" ", part)).strip()


def claim_units(body, keep_unlinked=False):
    """The sentences of an answer with the links they carry. A piece that holds little
    more than links, as "([docs](u))." after a full stop, cites the sentence before it;
    a piece starting with a small letter or a number was cut at an abbreviation
    ("art. 30", "n. 633") and goes on with it. With keep_unlinked the sentences without
    links stay too (free_report.py counts them); "chars" is how many characters of words
    a sentence has outside its links."""
    out = []
    last = ""
    for line in body.split("\n"):
        s = line.strip()
        if not s or s.startswith("#") or RULE.fullmatch(s):
            continue
        parts = [s] if s.startswith("|") else [p.strip() for p in judge.SENT_END.split(LIST_MARK.sub("", s)) if p.strip()]
        prev = None
        for part in parts:
            links, text, addresses = links_in(part), judge.clean_sentence(part), address_links(part)
            if prev is not None and (part[0].islower() or part[0].isdigit()):
                prev["text"] = (prev["text"] + " " + text).strip()
                prev["links"].extend(u for u in links if u not in prev["links"])
                prev["addresses"] |= addresses
                prev["chars"] += len(words_outside_links(part))
                last = prev["text"]
                continue
            if links and prev is not None and len(words_outside_links(part)) < MIN_CLAIM:
                prev["links"].extend(u for u in links if u not in prev["links"])
                prev["addresses"] |= addresses
                continue
            unit = {"text": text, "context": prev["text"] if prev else last, "links": list(dict.fromkeys(links)), "addresses": addresses,
                    "chars": len(words_outside_links(part))}
            out.append(unit)
            prev = unit
            if text:
                last = text
    return out if keep_unlinked else [u for u in out if u["links"]]


def page_classes(turn):
    """Normalized URL -> what happened to the page, from what Go saved with the answer."""
    loop = turn["meta"].get("loop") or {}
    out = {}
    for s in turn["sources"] or []:
        if isinstance(s, dict) and s.get("url"):
            out[ev.norm_url(s["url"])] = "preview"
    for it in (loop.get("review_items") or []) + (loop.get("review_confirmed") or []):
        if it.get("url") and not it.get("unread"):
            out[ev.norm_url(it["url"])] = "vedetta"
    for a in turn["meta"].get("anchors") or []:
        if a.get("basis") == "verified" and a.get("url"):
            out[ev.norm_url(a["url"])] = "vedetta"
    for u, state in (loop.get("link_states") or {}).items():
        if state in ("checked", "partial"):
            out[ev.norm_url(u)] = "vedetta"
    for c in loop.get("calls_detail") or []:
        if c.get("tool") == "read" and c.get("result") == "ok" and c.get("url"):
            out[ev.norm_url(c["url"])] = "read"
    return out


def load_turns(db, runs=None, where=None):
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    conn.text_factory = lambda b: b.decode("utf-8", "replace")
    q = ("select session_id, query, content, sources_json, meta_json, created_at from messages "
         "where role='assistant' and mode='deep' and meta_json like '%\"loop\"%' order by created_at")
    wanted = None
    if runs:
        with open(runs) as f:
            rows = json.load(f)
        if isinstance(rows, dict):  # runs_NAME.json of a bench round: {case: session}
            rows = [{"case": case, "sid": sid} for case, sid in rows.items()]
        if where:
            k, _, v = where.partition("=")
            rows = [r for r in rows if str(r.get(k)) == v]
        wanted = {r["sid"] for r in rows if r.get("sid")}
    turns = []
    for sid, query, content, sources, meta, created in conn.execute(q):
        if wanted is not None and sid not in wanted:
            continue
        m = json.loads(meta or "{}")
        if (m.get("loop") or {}).get("calls"):
            turns.append({"sid": sid, "query": query, "content": content, "sources": json.loads(sources or "[]") or [],
                          "meta": m, "created": created})
    return turns


def pairs_of(turn):
    """One (page, sentences) per page the answer links."""
    classes = page_classes(turn)
    pages = {}
    for unit in claim_units(answer_body(turn["content"])):
        for url in unit["links"]:
            n = ev.norm_url(url)
            p = pages.setdefault(n, {"sid": turn["sid"], "url": url, "cls": classes.get(n, "other"), "units": []})
            if not any(x["text"] == unit["text"] for x in p["units"]):
                row = {"text": unit["text"], "context": unit["context"],
                       "others": sum(1 for u in unit["links"] if u != url and u not in unit["addresses"])}
                if url in unit["addresses"]:
                    row["verdict"], row["by"] = "POINTER", "address"
                p["units"].append(row)
    return list(pages.values())


# ---------- pages ----------

def store_path(url):
    return os.path.join(STORE, hashlib.sha256(url.encode()).hexdigest() + ".json")


def read_with(tool, flag, url):
    """One reader through scripts/htmltext: (text, status)."""
    with tempfile.NamedTemporaryFile(suffix=".txt") as tmp:
        try:
            r = subprocess.run([tool, flag, url, tmp.name], capture_output=True, text=True, timeout=90)
            res = json.loads(r.stdout.strip().splitlines()[-1])
        except (subprocess.TimeoutExpired, ValueError, IndexError):
            return "", "timeout"
        if res.get("error"):
            return "", "error: " + res["error"][:120]
        if res.get("block"):
            return "", "blocked: " + res["block"]
        with open(tmp.name, errors="replace") as f:
            text = f.read()
    return text, "ok" if len(text.strip()) >= MIN_PAGE else "thin"


host_locks = collections.defaultdict(threading.Lock)
jina_slots = threading.Semaphore(2)


def fetch_page(tool, url):
    """The page as the home chain reads it today, kept in the store."""
    path = store_path(url)
    if os.path.exists(path):
        with open(path) as f:
            return json.load(f)
    with host_locks[ev.norm_url(url).split("/")[0]]:  # one request at a time per site
        text, status = read_with(tool, "-http", url)
        by = "http"
    if status != "ok":
        with jina_slots:
            jtext, jstatus = read_with(tool, "-jina", url)
        if jstatus == "ok":
            text, status, by = jtext, jstatus, "jina"
    entry = {"url": url, "status": status, "reader": by, "fetched": time.strftime("%Y-%m-%d %H:%M"), "text": text if status == "ok" else ""}
    os.makedirs(STORE, exist_ok=True)
    with open(path, "w") as f:
        json.dump(entry, f, ensure_ascii=False)
    return entry


def page_for(pair, cache_dir, tool, fetch=True):
    text = jp.cached_page(cache_dir, pair["url"]) if cache_dir else None
    if text:  # what the round saw, whoever opened it: the page cannot have changed since
        return text, "cache"
    if not fetch:
        return "", "not fetched"
    e = fetch_page(tool, pair["url"])
    return e["text"], e["status"]


# ---------- judging ----------

def message(page_url, text, units):
    lines = []
    for i, u in enumerate(units, 1):
        ctx = f" (context: {u['context']})" if u.get("context") else ""
        lines.append(f"{i}. {u['text']}{ctx}")
    return f"PAGE {page_url}:\n{text[:PAGE_CHARS]}\n\nSENTENCES:\n" + "\n".join(lines)


def parse_verdicts(raw, n):
    found = {}
    for m in LINE_RE.finditer(raw or ""):
        found[int(m.group(1))] = m.group(2).upper()
    return [found.get(i + 1, "ERROR") for i in range(n)]


def to_judge(pair):
    return [u for u in pair["units"] if u.get("by") != "address"]


def judge_pair(ask, pair):
    verdicts = []
    units = to_judge(pair)
    for start in range(0, len(units), BATCH):
        chunk = units[start:start + BATCH]
        verdicts += parse_verdicts(ask({"message": message(pair["url"], pair["text"], chunk)}), len(chunk))
    return verdicts


def pct(n, d):
    return round(100 * n / d, 1) if d else None


def summarize(pairs, single_only=False):
    """Counts per class of page. % SUPPORTED is over what could be judged, pointers apart.
    single_only keeps the sentences with one link: one with two sources is often only
    partly on each, and this is the fair comparison between pages read and not read."""
    table = {}
    for cls in CLASSES:
        c = collections.Counter()
        for p in pairs:
            if p["cls"] != cls:
                continue
            units = [u for u in p["units"] if u["others"] == 0] if single_only else p["units"]
            ok = p.get("status") in ("ok", "cache")
            c.update(u.get("verdict", "ERROR") if ok or u.get("by") == "address" else "UNREADABLE" for u in units)
        judged = sum(c[v] for v in ("SUPPORTED", "PARTIAL", "ABSENT", "CONTRADICTED"))
        table[cls] = {**{v: c[v] for v in VERDICTS + ("UNREADABLE", "ERROR")}, "judged": judged, "pct_supported": pct(c["SUPPORTED"], judged)}
    return table


BEST_FIRST = ("SUPPORTED", "PARTIAL", "CONTRADICTED", "ABSENT", "POINTER", "ERROR", "UNREADABLE")


def by_sentence(pairs):
    """Each sentence once, with the best verdict among the pages it links, split by
    whether at least one of them is a page the model read: what a reader who opens
    all its links would find, and whether the model had read any of them."""
    seen = {}
    for p in pairs:
        ok = p.get("status") in ("ok", "cache")
        for u in p["units"]:
            s = seen.setdefault((p["sid"], u["text"]), {"read": False, "verdicts": []})
            s["read"] = s["read"] or p["cls"] == "read"
            s["verdicts"].append(u.get("verdict", "ERROR") if ok or u.get("by") == "address" else "UNREADABLE")
    table = {"some page read": collections.Counter(), "no page read": collections.Counter()}
    for s in seen.values():
        best = next(v for v in BEST_FIRST if v in s["verdicts"])
        table["some page read" if s["read"] else "no page read"][best] += 1
    out = {}
    for k, c in table.items():
        judged = sum(c[v] for v in ("SUPPORTED", "PARTIAL", "ABSENT", "CONTRADICTED"))
        out[k] = {**{v: c[v] for v in BEST_FIRST}, "judged": judged, "pct_supported": pct(c["SUPPORTED"], judged)}
    return out


def pages_cut(pairs):
    """How many pages the judge sees only in part: longer than the limit."""
    return sum(1 for p in pairs if p.get("status") in ("ok", "cache") and len(p.get("text") or "") > PAGE_CHARS)


def format_summary(saved):
    """The tables of a judging, from what judgelinks_NAME.json keeps: the bench prints a
    judging already paid without calling the judge again (Piano dopo audit 5.1). The
    first line says how much of each page the judge saw: it is part of the ruler."""
    def row_text(label, width, row):
        return (f"  {label:<{width}} judged {row['judged']:>4}  SUPPORTED {row['SUPPORTED']:>4} ({row['pct_supported']}%)  PARTIAL {row['PARTIAL']:>4}  "
                f"ABSENT {row['ABSENT']:>4}  CONTRADICTED {row['CONTRADICTED']:>3}  | pointer {row['POINTER']:>3}  unreadable {row['UNREADABLE']:>3}  errors {row['ERROR']}")
    shown = saved.get("page_chars") or OLD_PAGE_CHARS
    lines = [f" the judge saw the first {shown} characters of each page"
             + (f" ({saved['pages_cut']} pages were longer)" if saved.get("pages_cut") is not None else "")
             + ("" if shown == PAGE_CHARS else f": not comparable with a judging on {PAGE_CHARS}")]
    for title, key in (("all sentences", "summary"), ("sentences with one link", "summary_one_link")):
        lines.append(f" {title}:")
        lines += [row_text(cls, 8, row) for cls, row in (saved.get(key) or {}).items()]
    lines.append(" each sentence once, best verdict among its links:")
    lines += [row_text(k, 15, row) for k, row in (saved.get("summary_sentences") or {}).items()]
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("name")
    ap.add_argument("--db", required=True)
    ap.add_argument("--cache", help="the cache of the round: the pages the model read")
    ap.add_argument("--runs", help="JSON list with a sid per run, or the runs file of a bench round; default: all the Deep a ciclo answers of DB")
    ap.add_argument("--where", help="keep the runs with this field, e.g. v=senza")
    ap.add_argument("--limit", type=int, default=0, help="only this many pages (a pilot)")
    ap.add_argument("--fetch-only", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--env", default=os.path.join(HERE, "judge.env"))
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--fetch-workers", type=int, default=8)
    args = ap.parse_args()

    tool = os.path.join(tempfile.gettempdir(), f"vedetta-htmltext-{os.getuid()}")
    if not args.dry_run:
        subprocess.run(["go", "build", "-o", tool, "./scripts/htmltext"], cwd=REPO, check=True)

    turns = load_turns(args.db, args.runs, args.where)
    pairs = [p for t in turns for p in pairs_of(t)]
    if args.limit:
        pairs = pairs[:: max(1, len(pairs) // args.limit)][:args.limit]
    print(f"{args.name}: {len(turns)} answers, {len(pairs)} (answer, page) pairs, {sum(len(p['units']) for p in pairs)} (sentence, page) pairs", flush=True)
    print("links by page:", dict(collections.Counter(p["cls"] for p in pairs)), "only addresses:", sum(1 for p in pairs if not to_judge(p)),
          "characters of answers:", sum(len(answer_body(t["content"])) for t in turns), flush=True)

    def attach(p):
        p["text"], p["status"] = page_for(p, args.cache, tool, fetch=not args.dry_run)
        return p
    with ThreadPoolExecutor(max_workers=max(1, args.fetch_workers)) as pool:
        list(pool.map(attach, pairs))
    status = collections.Counter(p["status"].split(":")[0] for p in pairs)
    print("pages:", dict(status), flush=True)

    todo = [p for p in pairs if p["status"] in ("ok", "cache") and to_judge(p)]
    calls = sum((len(to_judge(p)) - 1) // BATCH + 1 for p in todo)
    chars = sum(((len(to_judge(p)) - 1) // BATCH + 1) * min(len(p["text"]), PAGE_CHARS) for p in todo)
    print(f"judge would get: {calls} calls, {chars} page characters (~{chars // 4} tokens); "
          f"pages longer than {PAGE_CHARS} characters, cut: {pages_cut(todo)}", flush=True)
    if args.fetch_only or args.dry_run:
        return

    cfg = judge.judge_config(args.env, ev.DEFAULT_BASE)
    ask = judge.make_ask(cfg, timeout=600, system=SYSTEM_PROMPT, build=lambda it: it["message"], max_tokens=MAX_TOKENS)
    with ThreadPoolExecutor(max_workers=max(1, args.workers)) as pool:
        for p, verdicts in zip(todo, pool.map(lambda x: judge_pair(ask, x), todo)):
            for u, v in zip(to_judge(p), verdicts):
                u["verdict"] = v
    saved = {"name": args.name, "judge": cfg.get("model"), "page_chars": PAGE_CHARS, "pages_cut": pages_cut(todo), "summary": summarize(pairs), "summary_one_link": summarize(pairs, single_only=True),
             "summary_sentences": by_sentence(pairs), "pairs": [{k: v for k, v in p.items() if k != "text"} for p in pairs]}
    print(format_summary(saved))
    os.makedirs(OUT, exist_ok=True)
    path = os.path.join(OUT, f"judgelinks_{args.name}.json")
    with open(path, "w") as f:
        json.dump(saved, f, ensure_ascii=False, indent=1)
    print("written", path)


if __name__ == "__main__":
    main()
