#!/usr/bin/env python3
"""Vedetta - deterministic quality evaluator (F7.3/B, extended in V25-T1.2).

Evaluates a session against the machine-readable benchmark (scripts/benchmark.json):
PASS/FAIL for objective criteria + a rubric template left for human scoring.
Prefers the structured answer metadata persisted by the server (message "meta",
V25-T1.1) and falls back to parsing the answer text for older sessions.
Stdlib only (no dependencies).

Usage:
  python3 scripts/eval.py --session <id>
  python3 scripts/eval.py --case <benchmark-id>
  python3 scripts/eval.py --all [--suite reduced|smoke] [--pause SECONDS]
A case with "turns" in place of "query" is a conversation (Piano dopo audit 5.3): its
questions go one after another in the same session, and --turns-out keeps how long each took.
Options:
  --base-url URL    default http://localhost:3030
  --benchmark PATH  default scripts/benchmark.json
  --log FILE        append the reports to a file
"""
import argparse
import json
import os
import re
import sys
import time
import urllib.request

DEFAULT_BASE = "http://localhost:3030"
DEFAULT_BENCHMARK = os.path.join(os.path.dirname(os.path.abspath(__file__)), "benchmark.json")
# The questions with a known answer: real questions of the user, local only (docs/ is
# never versioned). Extra cases of the benchmark read their question from here.
DEFAULT_KNOWN_SET = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "docs", "domande_risposta_nota.json")


def select_cases(bench, suite):
    """(case ids to run, every case by id). Without a suite only the normal round's
    cases; a suite may also name the extra cases (Piano dopo audit 5.4)."""
    cases = {c["id"]: c for c in bench.get("cases", []) + bench.get("extra_cases", [])}
    if not suite:
        return [c["id"] for c in bench.get("cases", [])], cases
    selected = bench.get("suites", {}).get(suite)
    if not selected:
        sys.exit(f"unknown suite: {suite}")
    return list(selected), cases


def fill_known_queries(bench, path=DEFAULT_KNOWN_SET):
    """Give each extra case without a question the one of its known entry in the
    local set. A missing set or entry leaves the case without one (check_runnable)."""
    try:
        with open(path) as f:
            known = {c["id"]: c.get("q") for c in json.load(f).get("cases", [])}
    except (OSError, ValueError):
        known = {}
    for c in bench.get("extra_cases", []):
        if not c.get("query") and known.get(c.get("known")):
            c["query"] = known[c["known"]]
    return bench


def case_turns(case):
    """The questions of a case, in order: its turns, or its one question."""
    return list(case["turns"]) if "turns" in case else [case.get("query")]


def check_runnable(case):
    """Stop before a round when a case has no question to send."""
    if "turns" in case:
        if not case["turns"] or not all(case["turns"]):
            sys.exit(f"case {case['id']}: a conversation without questions")
        return
    if not case.get("query"):
        sys.exit(f"case {case['id']}: no question; its known entry {case.get('known')!r} "
                 f"is not in docs/domande_risposta_nota.json (local set)")


def access_headers():
    """Map R6: the access code, when Vedetta has one, from the environment or the
    repo .env; sent as a Bearer header (never printed)."""
    code = os.environ.get("VEDETTA_ACCESS_CODE", "").strip()
    if not code:
        env = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".env")
        try:
            for line in open(env, encoding="utf-8"):
                k, sep, v = line.partition("=")
                if sep and k.strip() == "VEDETTA_ACCESS_CODE" and not line.lstrip().startswith("#"):
                    code = v.strip().strip('"').strip("'")
        except OSError:
            pass
    return {"Authorization": "Bearer " + code} if code else {}


def http_json(url, timeout=30):
    req = urllib.request.Request(url, headers=access_headers())
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8"))


def run_query(base, case, timeout=None, session_id=None):
    """POST /api/search and return the session id parsed from the SSE stream.
    A Deep may take long: 45 minutes before giving up.
    With session_id the question is a follow-up in that session."""
    if timeout is None:
        timeout = 2700
    fields = {
        "query": case["query"],
        "mode": "deep",  # the only way since map R1; the server takes every mode as the Deep
        "answer_language": case.get("answer_language", ""),
    }
    if session_id:
        fields["session_id"] = session_id
    body = json.dumps(fields).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/api/search",
        data=body,
        headers={"Content-Type": "application/json", **access_headers()},
    )
    found = None
    with urllib.request.urlopen(req, timeout=timeout) as r:
        for raw in r:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:") or found is not None:
                continue
            try:
                d = json.loads(line[5:].strip())
            except Exception:
                continue
            if isinstance(d, dict) and d.get("session_id"):
                found = d["session_id"]
                # keep reading to EOF so the server completes the search
    if session_id and found and found != session_id:
        raise RuntimeError(f"the follow-up went to session {found}, not {session_id}")
    if not (found or session_id):
        raise RuntimeError("no session_id in SSE stream")
    return found or session_id


def run_case(base, case):
    """Run a case: (session id, one {"q", "seconds"} per question sent). A conversation
    sends its turns one after another in the same session; the next goes when the stream
    of the one before has ended."""
    sid, turns = None, []
    for q in case_turns(case):
        start = time.time()
        sid = run_query(base, {**case, "query": q}, session_id=sid)
        turns.append({"q": q, "seconds": round(time.time() - start)})
    return sid, turns


def fetch_session(base, session_id):
    return http_json(base.rstrip("/") + "/api/sessions/" + session_id)


def _as_list(v):
    if isinstance(v, list):
        return v
    if isinstance(v, str) and v.strip():
        try:
            return json.loads(v)
        except Exception:
            return []
    return []


# Headings that start the deterministic tail appended by the server after the
# writer's answer (methodology, bibliography, provenance note). Everything before
# the earliest match is the "body" the metrics look at. Keep in sync with
# internal/agent/labels.go, writer.go (BuildMethodologySection), reconciler.go
# (BuildBibliography), deep.go (fallback bibliography) and provenance.go.
TAIL_MARKERS = re.compile(
    r"^(?:"
    r"#{2,3}\s+(?:Note Metodologiche|Methodological Notes|Methodische Anmerkungen"
    r"|Notes M\u00e9thodologiques|Notas Metodol\u00f3gicas)"
    r"|#{2,3}\s+(?:Fonti e Riferimenti Verificati|Fonti Consultate|Anteprime di Ricerca Citate)"
    r"|>\s*\*\*(?:Provenienza|Provenance|Herkunft|Procedencia)\b"
    r"|<!-- vedetta:notes -->"  # labels.go NotesMarker: Vedetta's own notes, not claims to judge
    r")",
    re.MULTILINE,
)
PROVENANCE_TEXT = re.compile(r"\*\*(?:Provenienza|Provenance|Herkunft|Procedencia)\s*:\s*([A-Za-z]+)")
CITATION_NUM = re.compile(r"\[(\d+)\]")
SNIPPET_CITATION = re.compile(r"\[S\d+\]")
URL_IN_TEXT = re.compile(r"https?://[^\s\]\)<>\"']+")
NOT_FOUND_MARKERS = re.compile(
    r"non (?:sono|ho|abbiamo) (?:stat[ie] )?trovat|nessun[ao]? (?:fatt|evidenz|fonte|informazion|riscontr)"
    r"|non esiste (?:alcun|nessun)|non risulta(?:no)? (?:alcun|nessun|esistent|registrat)"
    r"|no (?:verified|reliable)? ?(?:evidence|facts|sources|information) (?:was |were )?found|not find any|could not find|no evidence"
    r"|no (?:entity|company|record|person|such) (?:named|called|exists|found)|does not (?:appear|exist) in|there is no (?:record|evidence|entity)"
    r"|keine (?:\w+ )?(?:belege|fakten|quellen|informationen)|nicht gefunden"
    r"|aucun(?:e)? (?:\w+ )?(?:preuve|fait|source|information)|pas trouv"
    r"|no se (?:han )?encontr|ning\u00fan (?:dato|hecho)|ninguna (?:fuente|evidencia|informaci\u00f3n)",
    re.IGNORECASE,
)


def answer_body(content):
    """The writer's answer without the server-appended tail."""
    m = TAIL_MARKERS.search(content)
    body = content[: m.start()] if m else content
    return re.sub(r"(?:\n\s*-{3,}\s*)+\Z", "", body.rstrip()).rstrip()


def norm_url(u):
    u = u.strip().rstrip(".,;:!?")
    u = re.sub(r"^https?://", "", u, flags=re.IGNORECASE)
    u = re.sub(r"^www\.", "", u, flags=re.IGNORECASE)
    u = u.split("#", 1)[0]
    host, _, path = u.partition("/")
    return (host.lower() + ("/" + path if path else "")).rstrip("/")


# V25-QA A2: Go's first sentence of an answer without verified facts (i18n.go "no_facts_lead").
NO_FACTS_LEADS = (
    "Non ho trovato fatti verificati nelle pagine lette per rispondere a questa domanda.",
    "I found no verified facts in the pages I read to answer this question.",
    "In den gelesenen Seiten habe ich keine überprüften Fakten gefunden, um diese Frage zu beantworten.",
    "Je n'ai trouvé aucun fait vérifié dans les pages lues pour répondre à cette question.",
    "No encontré hechos verificados en las páginas leídas para responder a esta pregunta.",
)

# Checks run on every case, on top of the case's own (V25-QA A2).
GLOBAL_CHECKS = [{"type": "no_facts_honest"}]


def canonical_numbers(text):
    """Numbers in text with thousands separators removed and decimal commas as dots.

    1.000 / 1,000 / 1000 -> "1000"; 12,5 / 12.5 -> "12.5"; 2019-2021 -> 2019, 2021.
    """
    t = re.sub(r"(?<=\d)[.,](?=\d{3}(?!\d))", "", text)
    t = re.sub(r"(?<=\d),(?=\d)", ".", t)
    return re.findall(r"\d+(?:\.\d+)?", t)


def substantive_paragraphs(body):
    """Prose paragraphs long enough to carry claims (no headings, tables, quotes)."""
    out = []
    for p in re.split(r"\n\s*\n", body):
        p = p.strip()
        if len(p) <= 200 or p.startswith(("#", "|", ">")):
            continue
        out.append(p)
    return out


def extract_signals(session, index=0):
    msgs = session.get("messages") or []
    turn = msgs[index] if msgs else {}
    subqueries = []
    for s in _as_list(turn.get("subqueries")):
        subqueries.append(s.get("query", "") if isinstance(s, dict) else str(s))
    facts = _as_list(turn.get("facts"))
    sources = _as_list(turn.get("sources"))
    content = turn.get("content") or ""
    meta = turn.get("meta") if isinstance(turn.get("meta"), dict) else None
    body = answer_body(content)

    domains = set()
    source_urls = set()
    for s in sources:
        u = s.get("url", "") if isinstance(s, dict) else ""
        m = re.match(r"https?://([^/]+)", u)
        if m:
            domains.add(m.group(1).lower())
        if u:
            source_urls.add(norm_url(u))

    if meta is not None:
        provenance = str(meta.get("provenance") or "").lower()
        cited_refs = meta.get("citations") or []
        cited_ids = [c.get("fact_id") for c in cited_refs if isinstance(c, dict)]
        citations = len(cited_refs)
    else:
        m = PROVENANCE_TEXT.search(content)
        provenance = m.group(1).lower() if m else ""
        nums = {int(x) for x in CITATION_NUM.findall(body)}
        citations = len(nums)
        cited_ids = None  # the number -> fact map is unknown for legacy sessions

    links = sorted({norm_url(u) for u in URL_IN_TEXT.findall(body)})
    # A link that differs from a source only in its query string (tracking params
    # such as srsltid, ?la=en) still points to the same page: not "outside".
    source_paths = {u.split("?", 1)[0] for u in source_urls}
    links_outside = [u for u in links if u not in source_urls and u.split("?", 1)[0] not in source_paths]

    paras = substantive_paragraphs(body)
    uncited = [p for p in paras if not (CITATION_NUM.search(p) or SNIPPET_CITATION.search(p) or URL_IN_TEXT.search(p))]

    by_id = {f.get("id"): f for f in facts if isinstance(f, dict)}
    checked = [by_id[i] for i in cited_ids if i in by_id] if cited_ids is not None else list(by_id.values())
    number_mismatch = []
    for f in checked:
        snippet_nums = set(canonical_numbers(f.get("verbatim_snippet", "")))
        missing = [n for n in canonical_numbers(f.get("claim", "")) if n not in snippet_nums]
        if missing:
            number_mismatch.append((f.get("id"), missing, f.get("claim", ""), f.get("verbatim_snippet", "")))

    return {
        "subqueries": subqueries,
        "facts": facts,
        "sources": sources,
        "content": content,
        "body": body,
        "domains": domains,
        "has_meta": meta is not None,
        # V25-T3.5 link checks done in the product (meta v2); None on older answers.
        "snippet_citations": len(meta.get("snippet_citations") or []) if meta else None,
        "dropped_phantom_urls": meta.get("dropped_phantom_urls") if meta else None,
        "repaired_urls": meta.get("repaired_urls") if meta else None,
        # I12 search conditions (meta v3); None on older answers.
        "search_queries": meta.get("search_queries") if meta and meta.get("v", 0) >= 3 else None,
        "empty_searches": meta.get("empty_searches") if meta else None,
        "unresponsive_engines": (meta.get("unresponsive_engines") or []) if meta else [],
        # V25-R1 search/page cache hits (meta v4); None when the cache is off or on older answers.
        "cache": meta.get("cache") if meta else None,
        "provenance": provenance,
        "citations": citations,
        "cited_fact_ratio": (citations / len(facts)) if facts else None,
        "paragraphs": len(paras),
        "uncited_paragraphs": uncited,
        "links": links,
        "links_outside": links_outside,
        "numbers_checked": len(checked),
        "number_mismatch": number_mismatch,
    }


def cache_summary(sigs):
    """One line with the cache hit rate over a run (V25-R1.7), or None when no
    session used the cache. A comparison between runs is valid only with a
    high hit rate."""
    stats = [s["cache"] for s in sigs if s.get("cache")]
    if not stats:
        return None
    modes = sorted({c.get("mode", "?") for c in stats})
    sh = sum(c.get("search_hits", 0) for c in stats)
    st = sh + sum(c.get("search_misses", 0) for c in stats)
    ph = sum(c.get("page_hits", 0) for c in stats)
    pt = ph + sum(c.get("page_misses", 0) for c in stats)

    def pct(hit, total):
        return f"{100 * hit / total:.0f}% hit ({hit}/{total})" if total else "n/a"

    line = f"cache: {'+'.join(modes)} - searches {pct(sh, st)}, pages {pct(ph, pt)}"
    if len(stats) < len(sigs):
        line += f" [{len(sigs) - len(stats)} sessions without cache data]"
    return line


def fact_claims(facts):
    out = []
    for f in facts:
        out.append(f.get("claim", "") if isinstance(f, dict) else str(f))
    return out


def run_check(chk, sig):
    t = chk.get("type")
    if t == "completes":
        return bool(sig["content"].strip()), f"content {len(sig['content'])} chars"
    if t == "subqueries_contain":
        val = chk["value"]
        ci = chk.get("case_insensitive", True)
        subs = sig["subqueries"]
        if not subs:
            return False, "no subqueries"
        def has(q):
            return (val.lower() in q.lower()) if ci else (val in q)
        missing = [q for q in subs if not has(q)]
        detail = f"{len(subs) - len(missing)}/{len(subs)} subqueries contain {val!r}"
        if missing:
            detail += f"; missing: {missing[:2]}"
        return (not missing), detail
    if t == "facts_exclude_keywords":
        kws = [k.lower() for k in chk["values"]]
        bad = [c for c in fact_claims(sig["facts"]) if any(k in c.lower() for k in kws)]
        return (not bad), (f"{len(bad)} off-topic facts" if bad else "no off-topic facts")
    if t == "min_facts":
        n = len(sig["facts"])
        return n >= chk["value"], f"facts={n} (min {chk['value']})"
    if t == "min_citations":
        n = sig["citations"]
        return n >= chk["value"], f"citations={n} (min {chk['value']})"
    if t == "provenance_in":
        allowed = [v.lower() for v in chk["values"]]
        return (sig["provenance"] in allowed), f"provenance={sig['provenance'] or '?'}"
    if t == "domains_include_any":
        vals = [v.lower() for v in chk["values"]]
        hit = sorted(d for d in sig["domains"] if any(v in d for v in vals))
        return bool(hit), (f"academic domains: {hit[:3]}" if hit else "none of the expected domains")
    if t == "cited_fact_ratio_min":
        r = sig["cited_fact_ratio"]
        if r is None:
            return False, "no facts extracted"
        return r >= chk["value"], f"cited/extracted facts = {r:.2f} (min {chk['value']})"
    if t == "uncited_paragraph_ratio_max":
        n = sig["paragraphs"]
        if n == 0:
            return None, "no substantive paragraphs"
        r = len(sig["uncited_paragraphs"]) / n
        return r <= chk["value"], f"uncited paragraphs {len(sig['uncited_paragraphs'])}/{n} = {r:.2f} (max {chk['value']})"
    if t == "links_in_sources":
        bad = sig["links_outside"]
        detail = f"{len(sig['links']) - len(bad)}/{len(sig['links'])} links belong to session sources"
        if bad:
            detail += f"; outside: {bad[:2]}"
        return (not bad), detail
    if t == "claim_numbers_in_snippet":
        n = sig["numbers_checked"]
        if n == 0:
            return None, "no facts to check"
        bad = len(sig["number_mismatch"])
        r = bad / n
        limit = chk.get("value", 0)
        return r <= limit, f"{bad}/{n} facts with claim numbers absent from their snippet (max ratio {limit})"
    if t == "answer_declares_not_found":
        if sig["provenance"] in ("parametric", "degraded"):
            return True, f"provenance={sig['provenance']}"
        hit = NOT_FOUND_MARKERS.search(sig["body"])
        return bool(hit), (f"declares: {hit.group(0)!r}" if hit else "no explicit 'not found' statement")
    if t == "no_facts_honest":
        if sig["facts"]:
            return None, "facts extracted: not a no-facts answer"
        if not sig["body"].strip():
            return None, "empty answer"
        lead = sig["body"].lstrip().startswith(NO_FACTS_LEADS)
        bad = sig["links_outside"]
        detail = ("opens with Go's no-facts sentence" if lead else "does not open with Go's no-facts sentence")
        if bad:
            detail += f"; links outside sources: {bad[:2]}"
        return (lead and not bad), detail
    if t == "answer_contains_any":
        low = sig["body"].lower()
        hit = [v for v in chk["values"] if v.lower() in low]
        return bool(hit), (f"found: {hit[:3]}" if hit else f"none of {chk['values'][:4]}")
    return None, f"unknown check type {t!r}"


def evaluate(case, session):
    """The signals and the checks of a case. For a conversation they are those of its last
    answer, plus one check: every turn got an answer."""
    turns = case.get("turns")
    sig = extract_signals(session, index=-1 if turns else 0)
    results = []
    for chk in case.get("checks", []) + GLOBAL_CHECKS:
        ok, detail = run_check(chk, sig)
        results.append((chk.get("type"), ok, detail))
    if turns:
        answered = sum(1 for m in session.get("messages") or []
                       if m.get("role", "assistant") == "assistant" and (m.get("content") or "").strip())
        results.append(("turns_answered", answered == len(turns), f"{answered}/{len(turns)} turns answered"))
    return sig, results


def render_report(case, session, sig, results):
    lines = []
    lines.append("=" * 68)
    lines.append(f"BENCHMARK CASE: {case.get('id')}  (mode={case.get('mode', 'standard')})")
    lines.append(f"session: {session.get('id')}  |  title: {(session.get('title') or '')[:70]}")
    lines.append("-" * 68)
    lines.append(
        f"signals: facts={len(sig['facts'])} citations={sig['citations']} "
        f"sources={len(sig['sources'])} provenance={sig['provenance'] or '?'} "
        f"meta={'yes' if sig['has_meta'] else 'no (text fallback)'}"
    )
    ratio = sig["cited_fact_ratio"]
    lines.append(
        "honesty: "
        f"cited_facts={'n/a' if ratio is None else f'{ratio:.2f}'} "
        f"uncited_paragraphs={len(sig['uncited_paragraphs'])}/{sig['paragraphs']} "
        f"links_outside_sources={len(sig['links_outside'])}/{len(sig['links'])} "
        f"claim_number_mismatch={len(sig['number_mismatch'])}/{sig['numbers_checked']}"
    )
    if sig["dropped_phantom_urls"] is not None:
        lines.append(
            f"links: snippet_citations={sig['snippet_citations']} "
            f"repaired_urls={sig['repaired_urls']} dropped_phantom_urls={sig['dropped_phantom_urls']}"
        )
    if sig["search_queries"] is not None:
        down = ", ".join(f"{e['engine']} ({e.get('reason') or 'no answer'} x{e['count']})" for e in sig["unresponsive_engines"])
        lines.append(
            f"search: queries={sig['search_queries']} empty={sig['empty_searches']} "
            f"unresponsive=[{down}]"
        )
    if sig["cache"]:
        c = sig["cache"]
        lines.append(
            f"cache: mode={c.get('mode')} searches={c.get('search_hits', 0)}/{c.get('search_hits', 0) + c.get('search_misses', 0)} "
            f"pages={c.get('page_hits', 0)}/{c.get('page_hits', 0) + c.get('page_misses', 0)}"
        )
    for fid, missing, claim, snippet in sig["number_mismatch"][:3]:
        lines.append(f"    {fid}: {missing} not in snippet | claim: {claim[:80]!r} | snippet: {snippet[:80]!r}")
    passed = 0
    total = 0
    for name, ok, detail in results:
        if ok is None:
            mark = "SKIP"
        else:
            mark = "PASS" if ok else "FAIL"
            total += 1
            passed += 1 if ok else 0
        lines.append(f"  [{mark}] {name:24s} - {detail}")
    lines.append("-" * 68)
    lines.append(f"deterministic: {passed}/{total} checks passed")
    lines.append("rubric (fill by hand): pertinenza=_/5  grounding=_/5  onesta=_/5  off-topic=_/5  formato=_/5")
    lines.append("=" * 68)
    return passed, total, "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description="Vedetta quality evaluator (F7)")
    ap.add_argument("--session", help="evaluate an existing session id")
    ap.add_argument("--case", help="run a benchmark case and evaluate it")
    ap.add_argument("--all", action="store_true", help="run and evaluate all benchmark cases")
    ap.add_argument("--suite", help="with --all: run only the cases of this suite (see 'suites' in the benchmark)")
    ap.add_argument("--runs-out", help="with --all: write the case -> session map to this JSON file")
    ap.add_argument("--turns-out", help="with --all: write the questions of each conversation and their seconds to this JSON file")
    ap.add_argument("--pause", type=float, default=0,
                    help="with --all: seconds to wait between cases (live smoke test, avoids rate limits)")
    ap.add_argument("--mode", choices=("deep",), default="deep",
                    help="every case runs as the Deep a ciclo, the only way since map R1 (17/11/2026)")
    ap.add_argument("--base-url", default=DEFAULT_BASE)
    ap.add_argument("--benchmark", default=DEFAULT_BENCHMARK)
    ap.add_argument("--log", help="append reports to this file")
    args = ap.parse_args()

    with open(args.benchmark) as f:
        bench = fill_known_queries(json.load(f))
    selected, cases = select_cases(bench, args.suite if args.all else None)
    if args.mode:
        cases = {cid: {**c, "mode": args.mode} for cid, c in cases.items()}

    runs = []
    conversations = {}  # case -> its turns, for the cases that are conversations
    if args.session:
        case = cases.get(args.case) if args.case else None
        if args.case and case is None:
            sys.exit(f"unknown case: {args.case}")
        runs.append((case, args.session))
    elif args.case:
        if args.case not in cases:
            sys.exit(f"unknown case: {args.case}")
        check_runnable(cases[args.case])
        sid, _ = run_case(args.base_url, cases[args.case])
        print(f"ran case {args.case} -> session {sid}")
        runs.append((cases[args.case], sid))
    elif args.all:
        for cid in selected:
            check_runnable(cases[cid])
        for i, cid in enumerate(selected):
            if i and args.pause > 0:
                time.sleep(args.pause)
            try:
                sid, turns = run_case(args.base_url, cases[cid])
            except Exception as e:  # one failing case must not stop a long run
                print(f"case {cid} FAILED to run: {e}", flush=True)
                continue
            print(f"ran case {cid} -> session {sid}", flush=True)
            runs.append((cases[cid], sid))
            if "turns" in cases[cid]:
                conversations[cid] = turns
        if args.runs_out:
            with open(args.runs_out, "w") as f:
                json.dump({c["id"]: sid for c, sid in runs}, f, indent=2)
        if args.turns_out and conversations:
            with open(args.turns_out, "w") as f:
                json.dump(conversations, f, indent=2, ensure_ascii=False)
    else:
        ap.error("one of --session/--case/--all is required")

    blocks = []
    sigs = []
    for case, sid in runs:
        session = fetch_session(args.base_url, sid)
        if case is None:
            case = {"id": "(session)", "mode": "-", "checks": [{"type": "completes"}]}
        sig, results = evaluate(case, session)
        sigs.append(sig)
        _, _, text = render_report(case, session, sig, results)
        print(text)
        blocks.append(text)

    summary = cache_summary(sigs)
    if summary:
        print(summary)
        blocks.append(summary)

    if args.log and blocks:
        with open(args.log, "a") as f:
            f.write("\n".join(blocks) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
