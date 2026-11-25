#!/usr/bin/env python3
"""Free report of bench rounds (Piano dopo audit 2.2): no judge, no network, read only.

Reads runs_NAME.json (scripts/out), the bench database and the round's cache
(~/vedetta-bench/data/cache-live-NAME) and prints, side by side when given several rounds:

  official page  among the results shown / read, for the entities of expected.json with a "home" (M4)
  sentences      with a link beside them (at least 25 characters of words outside links,
                 split as judge_links.claim_units), also by paragraph (M6)
  searches       by type (no quotes, one quoted phrase, two or more, site:) and how many came
                 back empty or relaxed (R12)
  reads          ok, unreadable, repeated; addresses that came from no search of the answer
                 (they may also be links read inside a page) (D2, R10)
  error pages    pages read, under 3,000 characters, whose title or opening says "not found" (R5)
  closing        how each answer closed, partial ones, drafts the sentinel sent back, rewrites
                 not finished, drafts that skipped the review (D3, D5)
  page language  pages read in the language of the search that found them: the language the
                 model gave, else the one its words show; searches of unclear language apart (3.3)

Every answer of a session counts. When a round holds conversations (cases with more than one
answer in their session, Piano dopo audit 5.3), a second table gives them turn by turn:

  seconds        of each turn, from turns_NAME.json (eval.py --turns-out)
  searches, pages read, and pages read again: already read in an earlier turn of the conversation
  sent back      answers whose draft the review sent back
  numbers        flagged in the note as found in no page, and how many of them are on a page
                 read in an earlier turn: the Deep a ciclo's own check (scripts/claimnumbers
                 -loop) on the text of those pages, from the round's cache
  notes          answers, and drafts, where the model wrote one of Vedetta's notes itself
  language       answers in the language the case asks for

Rows that need the cache say n/a when the round's cache is gone (replay rounds share
cache-run, rewritten by every prepare). Saves free_NAME.json beside the round files.

Usage:
  python3 scripts/free_report.py NAME [NAME ...] [--db PATH] [--cache-root DIR] [--out DIR]
"""
import argparse
import collections
import json
import os
import re
import sqlite3
import statistics
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import eval as ev  # noqa: E402
import judge_links as jl  # noqa: E402

REPO = os.path.dirname(HERE)
DEFAULT_OUT = os.path.join(HERE, "out")
DEFAULT_DB = os.path.expanduser("~/vedetta-bench/data/vedetta.db")
DEFAULT_CACHE_ROOT = os.path.expanduser("~/vedetta-bench/data")
DEFAULT_EXPECTED = os.path.join(HERE, "expected.json")

MIN_SENTENCE_CHARS = 25   # of words outside links: shorter pieces are not claims
ERROR_PAGE_CHARS = 3000   # a "not found" page is short
NOT_FOUND_RE = re.compile(r"\b404\b|not found|non trovat[ao]|nicht gefunden|introuvable|no encontrad[ao]|"
                          r"does(?:n't| not) exist|non esiste|n'existe pas|no existe", re.I)
QUOTED_RE = re.compile(r'"[^"]+"|“[^”]+”|«[^»]+»')
SEARCH_KEY_RE = re.compile(r'^v\d+\|search\|[^|]*\|(".*")\|lang=')
SEARCH_TYPES = ("plain", "one_quote", "more_quotes", "site")
# How Vedetta's own notes begin, in its five languages (internal/agent/i18n.go: loop_start,
# loop_note_links, loop_numbers_ok, loop_review_note_one, loop_pages_read,
# loop_note_reader_fallback). In what the model wrote, they are notes it copied or made up
# (defect of 06/11/2026 on follow-ups: must stay at zero).
NOTE_PHRASES_RE = re.compile("|".join(re.escape(p) for p in (
    "Ricerca guidata dal modello", "Model-driven research", "Vom Modell geführte Recherche",
    "Recherche guidée par le modèle", "Investigación guiada por el modelo",
    "Link nella risposta:", "Links in the answer:", "Links in der Antwort:", "Liens dans la réponse", "Enlaces en la respuesta:",
    "Numeri significativi della risposta", "Distinctive numbers in the answer", "Markante Zahlen der Antwort",
    "Nombres significatifs de la réponse", "Números significativos de la respuesta",
    "Prima di consegnarla, Vedetta", "Before delivering it, Vedetta", "Vor der Auslieferung hat Vedetta",
    "Avant de la livrer, Vedetta", "Antes de entregarla, Vedetta",
    "### Pagine lette", "### Pages read", "### Gelesene Seiten", "### Pages lues", "### Páginas leídas",
    "Alcune pagine sono state lette con", "Some pages were read with",
)))

# Function words, as DetectTextLanguage (i18n.go) does it: enough for a page, often not for a
# search of keywords, which then counts as of unclear language.
LANG_WORDS = {
    "it": set("il lo la gli le di del della dei delle per con che non una uno sono come quale quali tra nel nella più anche cosa è".split()),
    "en": set("the of and for with that which what how are is to in on a an from between does can this be".split()),
    "de": set("der die das und für mit ist nicht ein eine wie welche zwischen im auf von zu den dem".split()),
    "fr": set("le la les des du et pour avec que qui est une un dans sur pas comment quel quelle entre".split()),
    "es": set("el la los las del y para con que es una un en por como cual entre más".split()),
}


def detect_lang(text, least=2):
    toks = re.findall(r"\w+", (text or "").lower())
    score = sorted(((sum(t in w for t in toks), l) for l, w in LANG_WORDS.items()), reverse=True)
    if score[0][0] < least or score[0][0] == score[1][0]:
        return ""
    return score[0][1]


def load_expected(path):
    with open(path) as f:
        cases = json.load(f).get("cases") or {}
    return {c: (v.get("entities") or []) for c, v in cases.items()}


def norm_query(q):
    return " ".join((q or "").lower().split())


def search_type(q):
    if "site:" in (q or "").lower():
        return "site"
    n = len(QUOTED_RE.findall(q or ""))
    return "plain" if n == 0 else "one_quote" if n == 1 else "more_quotes"


def load_answers(db, runs):
    """case -> the answers of its session, in order: content, sources, meta. One for a
    question, one per turn for a conversation."""
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    conn.text_factory = lambda b: b.decode("utf-8", "replace")
    out = {}
    try:
        for case, sid in runs.items():
            rows = conn.execute("SELECT content, sources_json, meta_json FROM messages WHERE session_id = ? AND role = 'assistant' "
                                "ORDER BY created_at", (sid,)).fetchall()
            if rows:
                out[case] = [{"content": r[0] or "", "sources": json.loads(r[1] or "[]") or [], "meta": json.loads(r[2] or "{}") or {}}
                             for r in rows]
    finally:
        conn.close()
    return out


def load_languages(path):
    """case -> the language its answers are asked in, from the benchmark."""
    try:
        with open(path) as f:
            bench = json.load(f)
    except (OSError, ValueError):
        return {}
    return {c["id"]: c["answer_language"] for c in bench.get("cases", []) + bench.get("extra_cases", []) if c.get("answer_language")}


def load_seconds(out_dir, name):
    """case -> the seconds of each of its turns, from turns_NAME.json; {} for a round without it."""
    try:
        with open(os.path.join(out_dir, f"turns_{name}.json")) as f:
            return {case: [t.get("seconds") for t in turns] for case, turns in json.load(f).items()}
    except (OSError, ValueError):
        return {}


class LoopNumberCheck:
    """The Deep a ciclo's own number check (scripts/claimnumbers -loop), built at the first
    call: (sentences, text) -> the numbers of the sentences that are not in the text."""

    def __init__(self):
        self.tool = None

    def __call__(self, sentences, evidence):
        if self.tool is None:
            tool = os.path.join(tempfile.gettempdir(), f"vedetta-claimnumbers-{os.getuid()}")
            subprocess.run(["go", "build", "-o", tool, "./scripts/claimnumbers"], cwd=REPO, check=True, capture_output=True, timeout=600)
            self.tool = tool
        line = json.dumps({"id": "x", "claim": "\n".join(sentences), "evidence": evidence})
        done = subprocess.run([self.tool, "-loop"], input=line + "\n", capture_output=True, text=True, check=True, timeout=300)
        return json.loads(done.stdout.strip().splitlines()[-1])["missing"]


def turn_rows(answers, pages, seconds, languages, still_missing):
    """One row per turn over the conversations of a round: the cases with more than one answer.
    A number flagged at a turn counts as "on an earlier page" when the check no longer misses
    it against the pages read in the turns before (a page gone from the cache does not count)."""
    rows = []
    can_check = pages is not None and still_missing is not None
    for case, turns in answers.items():
        if len(turns) < 2:
            continue
        read_before = {}  # normalized URL -> text of the page, for the pages read in the turns before
        for i, a in enumerate(turns):
            if len(rows) <= i:
                rows.append({"turn": i + 1, "answers": 0, "seconds": [], "searches": 0, "pages_read": 0, "pages_read_again": 0,
                             "sent_back": 0, "numbers_flagged": 0, "numbers_on_earlier_pages": 0 if can_check else None,
                             "notes_in_answer": 0, "notes_in_draft": 0, "language_ok": 0, "language_known": 0})
            row = rows[i]
            loop = a["meta"].get("loop") or {}
            calls = loop.get("calls_detail") or []
            read_ok = list(dict.fromkeys(ev.norm_url(c["url"]) for c in calls
                                         if c.get("tool") == "read" and c.get("result") == "ok" and c.get("url")))
            row["answers"] += 1
            took = (seconds.get(case) or [])[i:i + 1]
            if took and took[0] is not None:
                row["seconds"].append(took[0])
            row["searches"] += sum(1 for c in calls if c.get("tool") == "search")
            row["pages_read"] += len(read_ok)
            row["pages_read_again"] += sum(1 for u in read_ok if u in read_before)
            row["sent_back"] += bool(loop.get("reviewed"))
            misses = loop.get("numbers_missing") or []
            flagged = collections.Counter(n for m in misses for n in m.get("missing") or [])
            row["numbers_flagged"] += sum(flagged.values())
            evidence = "\n".join(t for t in read_before.values() if t)
            if can_check and flagged and evidence and row["numbers_on_earlier_pages"] is not None:
                try:
                    still = collections.Counter(still_missing([m.get("sentence") or "" for m in misses], evidence))
                    row["numbers_on_earlier_pages"] += sum((flagged - still).values())
                except (OSError, ValueError, IndexError, KeyError, subprocess.SubprocessError):
                    can_check = False  # the check cannot run here: the row says n/a, never a wrong zero
            body = jl.answer_body(a["content"])
            row["notes_in_answer"] += bool(NOTE_PHRASES_RE.search(body))
            row["notes_in_draft"] += bool(NOTE_PHRASES_RE.search(loop.get("draft") or ""))
            want = (languages or {}).get(case)
            if want:
                row["language_known"] += 1
                row["language_ok"] += detect_lang(body) == want
            for u in read_ok:
                doc = (pages or {}).get(u) or {}
                read_before.setdefault(u, ((doc.get("title") or "") + "\n" + (doc.get("content") or "")).strip())
    for row in rows:
        took = row.pop("seconds")
        row["seconds_median"] = statistics.median(took) if took else None
        if not can_check:
            row["numbers_on_earlier_pages"] = None
    return rows


def round_cache_dir(name, cache_root):
    d = os.path.join(cache_root, f"cache-live-{name}")
    return d if os.path.isdir(d) else None


def load_cache(cache_dir):
    """(normalized query -> normalized result URLs, normalized URL -> document) of a cache folder."""
    searches, pages = collections.defaultdict(set), {}
    for kind in ("search", "page"):
        for root, _, files in os.walk(os.path.join(cache_dir, kind)):
            for fn in files:
                if not fn.endswith(".json"):
                    continue
                try:
                    with open(os.path.join(root, fn), encoding="utf-8", errors="replace") as f:
                        e = json.load(f)
                except (OSError, ValueError):
                    continue
                key = e.get("key") or ""
                if kind == "search":
                    m = SEARCH_KEY_RE.match(key)
                    if not m:
                        continue
                    try:
                        q = json.loads(m.group(1))
                    except ValueError:
                        q = m.group(1).strip('"')
                    for r in e.get("results") or []:
                        if r.get("url"):
                            searches[norm_query(q)].add(ev.norm_url(r["url"]))
                elif e.get("document") and key.startswith("v1|page|"):
                    pages[ev.norm_url(key[len("v1|page|"):])] = e["document"]
    return searches, pages


def has_home(url, homes):
    u = ev.norm_url(url)
    return any(h.lower().removeprefix("www.") in u for h in homes)


def sentence_counts(content):
    body = jl.answer_body(content)
    claims = [u for u in jl.claim_units(body, keep_unlinked=True) if u["chars"] >= MIN_SENTENCE_CHARS]
    paragraphs = paragraphs_linked = 0
    for par in re.split(r"\n\s*\n", body):
        units = [u for u in jl.claim_units(par, keep_unlinked=True) if u["chars"] >= MIN_SENTENCE_CHARS]
        if units:
            paragraphs += 1
            paragraphs_linked += any(u["links"] for u in units)
    return len(claims), sum(1 for u in claims if u["links"]), paragraphs, paragraphs_linked


def norm_sentence(s):
    """A sentence as Go saved it and as claim_units gives it, made comparable: no spaces of
    difference, no emphasis marks, lower case."""
    return re.sub(r"\s+", " ", re.sub(r"[*_`]", "", s or "")).strip().lower()


def quote_counts(loop, content):
    """The quote turn of one answer (Piano dopo audit 6.3): sentences asked and their states, and the
    `none` ones in three kinds: a link to a page Vedetta opened for the link check, a follow-up with pages
    of the earlier turns (the model did not have them), the others. Go saves each sentence without the
    addresses of its links (linkClaim.Text): the links come from the answer's text, where the sentence is
    found again; one not found goes with the others and is counted (unmatched)."""
    items = loop.get("quote_items") or []
    states = collections.Counter(it.get("state") for it in items)
    opened = {ev.norm_url(u) for u, st in (loop.get("link_states") or {}).items() if st in ("checked", "partial")}
    links_of = {}
    for u in jl.claim_units(jl.answer_body(content), keep_unlinked=True):
        links_of.setdefault(norm_sentence(u["text"]), []).extend(u["links"])
    kinds = collections.Counter()
    for it in items:
        if it.get("state") != "none":
            continue
        links = links_of.get(norm_sentence(it.get("sentence")))
        if links is None:
            kinds["other"] += 1
            kinds["unmatched"] += 1
        elif any(ev.norm_url(u) in opened for u in links):
            kinds["link_to_opened_page"] += 1
        elif (loop.get("session_pages") or 0) > 0:
            kinds["earlier_pages"] += 1
        else:
            kinds["other"] += 1
    return states, kinds


def pct(num, den):
    return round(100.0 * num / den, 1) if den else None


def compute_round(name, out_dir, db, cache_root, expected, languages=None, still_missing=None):
    with open(os.path.join(out_dir, f"runs_{name}.json")) as f:
        runs = json.load(f)
    by_case = load_answers(db, runs)
    answers = [(case, i, a) for case, turns in by_case.items() for i, a in enumerate(turns)]
    cache_dir = round_cache_dir(name, cache_root)
    searches_cache, pages = load_cache(cache_dir) if cache_dir else (None, None)

    official = {"pairs": 0, "in_results": 0, "read": 0}
    sent = {"claims": 0, "linked": 0, "paragraphs": 0, "paragraphs_linked": 0}
    per_answer = []
    searches = {t: {"n": 0, "empty": 0} for t in SEARCH_TYPES}
    relaxed = 0
    reads = collections.Counter()
    outside = 0 if cache_dir else None
    outside_ok = 0 if cache_dir else None
    error_pages = 0 if cache_dir else None
    lang_match = {"pages": 0, "same": 0, "query_lang_unknown": 0} if cache_dir else None
    closed_by = collections.Counter()
    partial = reviewed = unfinished = skipped = 0
    quote_states, quote_kinds, quote_answers, quote_seconds = collections.Counter(), collections.Counter(), collections.Counter(), []

    for case, turn, a in answers:
        meta, loop = a["meta"], a["meta"].get("loop") or {}
        calls = loop.get("calls_detail") or []
        read_ok = [c["url"] for c in calls if c.get("tool") == "read" and c.get("result") == "ok" and c.get("url")]
        shown = [s["url"] for s in a["sources"] if isinstance(s, dict) and s.get("url")]
        for ent in expected.get(case, []) if turn == 0 else []:  # the entities belong to the first question
            homes = ent.get("home") or []
            if not homes:
                continue
            official["pairs"] += 1
            official["in_results"] += any(has_home(u, homes) for u in shown)
            official["read"] += any(has_home(u, homes) for u in read_ok)

        claims, linked, pars, pars_linked = sentence_counts(a["content"])
        for k, v in zip(("claims", "linked", "paragraphs", "paragraphs_linked"), (claims, linked, pars, pars_linked)):
            sent[k] += v
        if claims:
            per_answer.append(100.0 * linked / claims)

        found = set()
        found_by = {}  # normalized URL -> the last search whose results held it
        for c in calls:
            if c.get("tool") == "search":
                if searches_cache is not None:
                    for u in searches_cache.get(norm_query(c.get("query")), set()):
                        found_by[u] = c
                t = searches[search_type(c.get("query"))]
                t["n"] += 1
                t["empty"] += c.get("result") == "empty"
                relaxed += c.get("result") == "relaxed"
                if searches_cache is not None:
                    found |= searches_cache.get(norm_query(c.get("query")), set())
            elif c.get("tool") == "read":
                reads[c.get("result") or "?"] += 1
                if searches_cache is not None and c.get("result") != "repeated" and c.get("url") \
                        and ev.norm_url(c["url"]) not in found:
                    outside += 1
                    outside_ok += c.get("result") == "ok"
        if pages is not None:
            for u in dict.fromkeys(read_ok):
                search = found_by.get(ev.norm_url(u))
                doc = pages.get(ev.norm_url(u))
                if search is None or not doc:
                    continue
                page_lang = detect_lang((doc.get("title") or "") + " " + (doc.get("content") or "")[:3000])
                query_lang = (search.get("lang") or "").lower()[:2] or detect_lang(search.get("query"), least=1)
                if not page_lang:
                    continue
                if not query_lang:
                    lang_match["query_lang_unknown"] += 1
                    continue
                lang_match["pages"] += 1
                lang_match["same"] += query_lang == page_lang
            for u in dict.fromkeys(read_ok):
                doc = pages.get(ev.norm_url(u))
                if doc and len((doc.get("content") or "").strip()) < ERROR_PAGE_CHARS and \
                        NOT_FOUND_RE.search((doc.get("title") or "") + " " + (doc.get("content") or "")[:300]):
                    error_pages += 1

        if loop.get("quotes"):
            quote_answers[loop["quotes"]] += 1
            st, kinds = quote_counts(loop, a["content"])
            quote_states.update(st)
            quote_kinds.update(kinds)
            if loop.get("quote_seconds"):
                quote_seconds.append(loop["quote_seconds"])
        closed_by[loop.get("closed_by") or "?"] += 1
        partial += bool(meta.get("partial"))
        reviewed += bool(loop.get("reviewed"))
        unfinished += bool(loop.get("review_unfinished"))
        skipped += bool(loop.get("review_skipped"))

    sent["pct"] = pct(sent["linked"], sent["claims"])
    sent["paragraphs_pct"] = pct(sent["paragraphs_linked"], sent["paragraphs"])
    sent["median_answer_pct"] = round(statistics.median(per_answer), 1) if per_answer else None
    searches["relaxed"] = relaxed
    other = sum(n for k, n in reads.items() if k not in ("ok", "unreadable", "repeated"))
    return {
        "name": name, "answers": len(answers), "missing": sorted(set(runs) - set(by_case)), "cache": cache_dir,
        "turns": turn_rows(by_case, pages, load_seconds(out_dir, name), languages, still_missing),
        "official": official, "sentences": sent, "searches": searches,
        "reads": {"ok": reads["ok"], "unreadable": reads["unreadable"], "repeated": reads["repeated"], "other": other,
                  "outside_results": outside, "outside_results_ok": outside_ok},
        "error_pages": error_pages,
        "lang_match": lang_match,
        "quotes": {"asked": sum(quote_states[k] for k in ("found", "not_found", "short", "none")), "found": quote_states["found"],
                   "not_found": quote_states["not_found"], "short": quote_states["short"], "none": quote_states["none"], "unasked": quote_states["unasked"],
                   "answers": dict(quote_answers),
                   "seconds_median": statistics.median(quote_seconds) if quote_seconds else None,
                   "none_kinds": {k: quote_kinds[k] for k in ("link_to_opened_page", "earlier_pages", "other")},
                   "none_unmatched": quote_kinds["unmatched"]},
        "closing": {"answers": len(answers), "closed_by": dict(closed_by), "partial": partial, "reviewed": reviewed,
                    "review_unfinished": unfinished, "review_skipped": skipped},
    }


def frac(num, den):
    if num is None:
        return "n/a"
    p = pct(num, den)
    return f"{num}/{den} ({p:.1f}%)" if p is not None else f"{num}/{den}"


def format_table(rounds):
    rows = [("answers", lambda r: str(r["answers"]) + (f" (missing {len(r['missing'])})" if r["missing"] else ""))]
    rows += [
        ("official page among results", lambda r: frac(r["official"]["in_results"], r["official"]["pairs"])),
        ("official page read", lambda r: frac(r["official"]["read"], r["official"]["pairs"])),
        ("sentences with a link", lambda r: frac(r["sentences"]["linked"], r["sentences"]["claims"])),
        ("paragraphs with a link", lambda r: frac(r["sentences"]["paragraphs_linked"], r["sentences"]["paragraphs"])),
        ("median per answer", lambda r: "n/a" if r["sentences"]["median_answer_pct"] is None
         else f"{r['sentences']['median_answer_pct']:.1f}%"),
    ]
    for t, label in (("plain", "searches without quotes"), ("one_quote", "searches, one quoted phrase"),
                     ("more_quotes", "searches, two or more quoted"), ("site", "searches with site:")):
        rows.append((label + ": empty", lambda r, t=t: frac(r["searches"][t]["empty"], r["searches"][t]["n"])))
    rows += [
        ("searches relaxed", lambda r: str(r["searches"]["relaxed"])),
        ("reads ok", lambda r: str(r["reads"]["ok"])),
        ("reads unreadable", lambda r: str(r["reads"]["unreadable"])),
        ("reads repeated", lambda r: str(r["reads"]["repeated"])),
        ("reads from no search (opened)", lambda r: "n/a" if r["reads"]["outside_results"] is None
         else f"{r['reads']['outside_results']} ({r['reads']['outside_results_ok']})"),
        ("error pages read", lambda r: "n/a" if r["error_pages"] is None else str(r["error_pages"])),
        ("pages in their search's language", lambda r: "n/a" if not r.get("lang_match")
         else frac(r["lang_match"]["same"], r["lang_match"]["pages"]) + f" (+{r['lang_match']['query_lang_unknown']} unclear)"),
        ("sentences with a quote found", lambda r: "n/a" if not r["quotes"]["asked"]
         else frac(r["quotes"]["found"], r["quotes"]["asked"])
         + f" (not found {r['quotes']['not_found']}, short {r['quotes'].get('short', 0)}, none {r['quotes']['none']}, unasked {r['quotes']['unasked']})"),
        ("  quote turn, answers by outcome", lambda r: "n/a" if not r["quotes"]["answers"]
         else ", ".join(f"{k} {v}" for k, v in sorted(r["quotes"]["answers"].items()))),
        ("  quote turn, seconds (median)", lambda r: "n/a" if r["quotes"]["seconds_median"] is None
         else f"{r['quotes']['seconds_median']:g}"),
        ("  none: link to a page Vedetta opened / earlier turns / others", lambda r: "n/a" if not r["quotes"]["none"]
         else "{link_to_opened_page} / {earlier_pages} / {other}".format(**r["quotes"]["none_kinds"])
         + f" (not found in the answer: {r['quotes'].get('none_unmatched', 0)})"),
        ("closed by", lambda r: ", ".join(f"{k} {v}" for k, v in sorted(r["closing"]["closed_by"].items()))),
        ("partial", lambda r: str(r["closing"]["partial"])),
        ("drafts sent back", lambda r: str(r["closing"]["reviewed"])),
        ("rewrites not finished", lambda r: str(r["closing"]["review_unfinished"])),
        ("drafts that skipped review", lambda r: str(r["closing"]["review_skipped"])),
    ]
    width = max(len(label) for label, _ in rows) + 2
    cells = [[fn(r) for r in rounds] for _, fn in rows]
    col = max([len(r["name"]) for r in rounds] + [len(c) for line in cells for c in line]) + 2
    lines = [" " * width + "".join(r["name"].ljust(col) for r in rounds)]
    for (label, _), line in zip(rows, cells):
        lines.append(label.ljust(width) + "".join(c.ljust(col) for c in line))
    return "\n".join(l.rstrip() for l in lines)


def format_turns(rounds):
    """The conversations of each round, turn by turn; empty when no round has any."""
    def cell(v):
        return "n/a" if v is None else f"{v:g}" if isinstance(v, float) else str(v)
    rows = [
        ("answers", lambda t: t["answers"]),
        ("seconds, median", lambda t: t["seconds_median"]),
        ("searches", lambda t: t["searches"]),
        ("pages read", lambda t: t["pages_read"]),
        ("pages read again (read in an earlier turn)", lambda t: t["pages_read_again"]),
        ("answers sent back by the review", lambda t: t["sent_back"]),
        ("numbers flagged in the note", lambda t: t["numbers_flagged"]),
        ("  of those, on a page read in an earlier turn", lambda t: t["numbers_on_earlier_pages"]),
        ("answers where the model wrote a note of Vedetta", lambda t: t["notes_in_answer"]),
        ("drafts where it did", lambda t: t["notes_in_draft"]),
        ("answers in the asked language", lambda t: f"{t['language_ok']}/{t['language_known']}" if t["language_known"] else None),
    ]
    width = max(len(label) for label, _ in rows) + 2
    blocks = []
    for r in rounds:
        turns = r.get("turns") or []
        if not turns:
            continue
        lines = [f"conversations of {r['name']}, by turn".ljust(width) + "".join(str(t["turn"]).ljust(10) for t in turns)]
        for label, fn in rows:
            lines.append(label.ljust(width) + "".join(cell(fn(t)).ljust(10) for t in turns))
        blocks.append("\n".join(l.rstrip() for l in lines))
    return "\n\n".join(blocks)


def save_round(r, out_dir):
    with open(os.path.join(out_dir, f"free_{r['name']}.json"), "w") as f:
        json.dump(r, f, indent=2, ensure_ascii=False)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("names", nargs="+", help="round names (runs_NAME.json)")
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--cache-root", default=DEFAULT_CACHE_ROOT, help="folder with cache-live-NAME")
    ap.add_argument("--out", default=DEFAULT_OUT)
    ap.add_argument("--expected", default=DEFAULT_EXPECTED)
    ap.add_argument("--benchmark", default=ev.DEFAULT_BENCHMARK, help="where each case says the language of its answers")
    args = ap.parse_args()
    expected = load_expected(args.expected)
    languages, check = load_languages(args.benchmark), LoopNumberCheck()
    rounds = [compute_round(n, args.out, args.db, args.cache_root, expected, languages, check) for n in args.names]
    for r in rounds:
        save_round(r, args.out)
    print(format_table(rounds))
    by_turn = format_turns(rounds)
    if by_turn:
        print("\n" + by_turn)


if __name__ == "__main__":
    main()
