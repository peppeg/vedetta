#!/usr/bin/env python3
"""Vedetta - LLM faithfulness judge for cited sentences (V25-T1.3).

For every sentence of an answer that carries citations, asks a judge model a
deliberately narrow question: "does the cited evidence support this sentence?"
The judge sees only the sentence, the previous sentence (to resolve references
like "this value") and the excerpts the answer cites as proof. It never sees the
user question, the pipeline or the model that wrote the answer: the point is to
check whether each sentence stands on the evidence a reader can verify.

Evidence per citation:
  [n]        -> the verbatim snippet of the cited fact (meta citation map, or the
                bibliography of older sessions)
  URL links  -> the passage of the page that Go anchored to the sentence when the
                page was read (Deep a ciclo, meta "anchors"), else the search-result
                snippet of that source (Tier 1, not full text)

Judge configuration (first found wins):
  1. environment variables JUDGE_BASE_URL, JUDGE_MODEL, JUDGE_API_KEY
     (optional JUDGE_REASONING_EFFORT, default "low")
  2. scripts/judge.env with the same KEY=VALUE lines (gitignored)
  3. Vedetta's default LLM endpoint from GET /api/config, without API key and
     with reasoning_effort "none" (the model judges its own family: less
     independent, see docs)
Results are only comparable when produced by the same judge model.

Usage:
  python3 scripts/judge.py <session-id> [<session-id> ...]
Options:
  --base-url URL   Vedetta server (default http://localhost:3030)
  --env FILE       judge env file (default scripts/judge.env)
  --workers N      parallel judge calls (default 4)
  --out DIR        where JSON results are written (default scripts/out)
  --evidence MODE  "anchor" (default): passages of pages read where Go saved them;
                   "preview": search previews only, as before (control run)
  --claims on|off  "on" (default): sentences without citations are judged too, on
                   the closest passage of what was read (Go's anchor in the Deep a
                   ciclo; the two closest fact passages or previews otherwise), and
                   reported apart as "every claim"; "off": listed, not judged
Stdlib only.
"""
import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import eval as ev  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_ENV = os.path.join(HERE, "judge.env")
DEFAULT_OUT = os.path.join(HERE, "out")

VERDICTS = ("SUPPORTED", "PARTIAL", "UNSUPPORTED")

SYSTEM_PROMPT = """You are a strict fact-checking judge.
Decide whether the EXCERPT supports the SENTENCE. Use only the excerpt: no outside knowledge, even if you know the sentence is true.
- SUPPORTED: every factual detail of the sentence (numbers, dates, names, quantities, qualifiers such as "always", "only", "most") is stated in or directly implied by the excerpt.
- PARTIAL: the excerpt supports the main point, but the sentence adds details, numbers, conversions or qualifiers that the excerpt does not contain.
- UNSUPPORTED: the excerpt does not support the sentence, or contradicts it.
The CONTEXT line only helps you understand what the sentence refers to; do not judge it.
The texts may be in different languages. Answer with exactly one word: SUPPORTED, PARTIAL or UNSUPPORTED."""

CITE_NUM = re.compile(r"\[(\d+)\]")
SNIP_NUM = re.compile(r"\[S(\d+)\]")
FACT_TAG = re.compile(r"\[\[fact:[^\]]*\]\]")
BIB_ENTRY = re.compile(r"^\[(\d+)\]\s+\*\*\[.*?\]\((\S+?)\)\*\*\s*\n>\s*\*«(.*?)»\*", re.MULTILINE | re.DOTALL)
# Cited search previews (V25-T3.5): "[S1] **[title](url)**" plus the preview text.
SNIP_BIB_ENTRY = re.compile(r"^\[S(\d+)\]\s+\*\*\[.*?\]\((\S+?)\)\*\*(?:\s*\n>\s*\*«(.*?)»\*)?", re.MULTILINE | re.DOTALL)
# V25-QA A5: Go's label in front of a deduction the writer declared (internal/agent/i18n.go
# "deduction_label"). Such a sentence goes beyond its sources on purpose: not judged.
DECLARED_LABEL = re.compile(
    r"^[\s>*_]*(?:\[[ xX]\]\s*)?[\s*_]*(?:Deduzione, non scritta nelle fonti|Inference, not stated in the sources|"
    r"Schlussfolgerung, nicht in den Quellen|Déduction, absente des sources|Deducción, no escrita en las fuentes)\s*:")
SENT_END = re.compile(r"(?<=[.!?;])\s+(?=[\"'«(\[*]*[A-ZÀ-ÖØ-Þ0-9])")


# ---------------------------------------------------------------- answer parsing

def split_sentences(body):
    """Split a markdown answer into judgeable units.

    Paragraph lines and bullet items are split on sentence boundaries; table rows
    are kept whole; headings and rules are dropped.
    """
    out = []
    for line in body.split("\n"):
        s = line.strip()
        if not s or s.startswith("#") or re.fullmatch(r"[-*_|:\s]{3,}", s):
            continue
        if s.startswith("|"):
            cells = [c.strip() for c in s.strip("|").split("|")]
            out.append(" — ".join(c for c in cells if c))
            continue
        s = re.sub(r"^(?:[-*+]|\d+[.)])\s+", "", s)
        out.extend(p.strip() for p in SENT_END.split(s) if p.strip())
    return out


def citations_in(sentence):
    """Citation numbers, URLs and snippet numbers ([S1]) carried by a sentence."""
    nums = sorted({int(n) for n in CITE_NUM.findall(sentence)})
    urls = sorted({u.rstrip(".,;:!?") for u in ev.URL_IN_TEXT.findall(sentence)})
    snips = sorted({int(n) for n in SNIP_NUM.findall(sentence)})
    return nums, urls, snips


def clean_sentence(sentence):
    """The sentence as a reader sees it, without citation markers and links."""
    s = FACT_TAG.sub("", sentence)
    s = re.sub(r"\[\[(https?://[^\]]+)\]\]", "", s)
    s = re.sub(r"\[(https?://[^\]]+)\]", "", s)
    s = re.sub(r"\[([^\]]+)\]\((https?://[^)]+)\)", r"\1", s)
    s = ev.URL_IN_TEXT.sub("", s)
    s = CITE_NUM.sub("", s)
    s = SNIP_NUM.sub("", s)
    s = s.replace("**", "").replace("__", "")
    return re.sub(r"\s+", " ", s).strip()


def fact_evidence(f):
    """The evidence the writer had for a fact: the source passage when present
    (V25-T3.4), else the aligned page text (T3.1), else the model's snippet.
    Returns (text, basis)."""
    for key in ("passage", "evidence", "verbatim_snippet"):
        text = (f.get(key) or "").strip()
        if text:
            return text, key
    return "", None


def norm_words(text):
    """Lower-case letters and digits only, for matching a sentence across splitters."""
    return re.sub(r"[^0-9a-zà-öø-ÿ]+", " ", (text or "").lower()).split()


def anchor_map(turn):
    """normalized url -> anchors of the Deep a ciclo [(sentence words, passage, basis)]."""
    meta = turn.get("meta") if isinstance(turn.get("meta"), dict) else {}
    out = {}
    for a in meta.get("anchors") or []:
        if a.get("linked") is False:
            continue
        if a.get("url") and a.get("passage"):
            out.setdefault(ev.norm_url(a["url"]), []).append((norm_words(a.get("sentence")), a["passage"],
                                                              a.get("basis") or "page"))
    return out


def best_anchor(anchors, sentence):
    """The anchor whose sentence shares most words with sentence (the judge and Go
    split sentences a little differently)."""
    words = set(norm_words(sentence))
    best, best_score = None, -1.0
    for a_words, passage, basis in anchors:
        aw = set(a_words)
        score = len(words & aw) / max(1, len(words | aw))
        if score > best_score:
            best, best_score = (passage, basis), score
    return best


def claim_anchors(turn):
    """Go's closest passages for each claim of a Deep a ciclo answer:
    [(sentence words, [(passage, basis, url), ...])], one entry per sentence."""
    meta = turn.get("meta") if isinstance(turn.get("meta"), dict) else {}
    out = []
    for a in meta.get("anchors") or []:
        if a.get("linked") is not False or not a.get("passage"):
            continue
        words = norm_words(a.get("sentence"))
        if out and out[-1][0] == words:
            out[-1][1].append((a["passage"], a.get("basis") or "page", a.get("url", "")))
        else:
            out.append((words, [(a["passage"], a.get("basis") or "page", a.get("url", ""))]))
    return out


STRONG_WORD = re.compile(r"[0-9A-ZÀ-ÖØ-Þ]|[a-zà-ÿ][A-Z]")


def claim_tokens(sentence):
    """Words of a claim with their weight, as Go's claimTokens: names and numbers
    weigh 3 (they read the same across languages), other words of 4+ letters 1."""
    out = {}
    for w in re.findall(r"[\w.-]+", sentence or ""):
        w = w.strip(".-")
        lw = w.lower()
        if not w:
            continue
        if STRONG_WORD.search(w) and len(lw) >= 2:
            out[lw] = 3
        elif len(lw) >= 4 and lw not in out:
            out[lw] = 1
    return out


def claim_pool(turn):
    """What the pipeline read, for the closest passages of a claim: fact passages
    and search previews, as [(lower text, text, basis, url)]."""
    pool = []
    for f in ev._as_list(turn.get("facts")):
        if isinstance(f, dict):
            text, basis = fact_evidence(f)
            if text:
                pool.append((text.lower(), text, basis, f.get("source_url", "")))
    for src in ev._as_list(turn.get("sources")):
        if isinstance(src, dict) and src.get("url"):
            text = " ".join(x for x in (src.get("title", ""), src.get("content", "")) if x).strip()
            if text:
                pool.append((text.lower(), text, "preview", src["url"]))
    return pool


def closest_evidence(sentence, anchors, pool, n=2):
    """The closest passages of what was read: Go's for a Deep a ciclo answer (matched
    by sentence), else the n pool texts with the highest weighted overlap."""
    if anchors:
        words = set(norm_words(sentence))
        best, best_score = None, 0.0
        for a_words, passages in anchors:
            aw = set(a_words)
            score = len(words & aw) / max(1, len(words | aw))
            if score > best_score:
                best, best_score = passages, score
        if not best or best_score < 0.5:
            return []
        return [{"ref": "closest", "kind": "closest", "text": p, "basis": b, "url": u} for p, b, u in best[:n]]
    tokens = claim_tokens(sentence)
    scored = []
    for i, (lower, _, _, _) in enumerate(pool):
        score = sum(w for t, w in tokens.items() if t in lower)
        if score:
            scored.append((score, -i))
    scored.sort(reverse=True)
    return [{"ref": "closest", "kind": "closest", "text": pool[-i][1], "basis": pool[-i][2], "url": pool[-i][3]}
            for _, i in scored[:n]]


def paragraphs(body):
    """Blocks of the answer: lines up to a blank line, a heading or a new list item,
    so a description and the line with its link stay together."""
    blocks, cur = [], []
    for line in body.split("\n"):
        s = line.strip()
        new_item = re.match(r"^(?:[-*+]|\d+[.)])\s+", s) or s.startswith("#") or s.startswith("|")
        if not s or new_item:
            if cur:
                blocks.append("\n".join(cur))
            cur = [line] if s else []
            continue
        cur.append(line)
    if cur:
        blocks.append("\n".join(cur))
    return blocks


def evidence_maps(turn):
    """Return (citation number -> fact evidence, normalized url -> search snippet,
    snippet number -> search snippet)."""
    facts = {f.get("id"): f for f in ev._as_list(turn.get("facts")) if isinstance(f, dict)}
    by_num = {}
    meta = turn.get("meta") if isinstance(turn.get("meta"), dict) else None
    if meta and meta.get("citations"):
        for c in meta["citations"]:
            f = facts.get(c.get("fact_id"))
            text, basis = fact_evidence(f) if f else ("", None)
            if text:
                by_num[int(c["n"])] = {"text": text, "url": c.get("source_url", ""),
                                       "fact_id": f.get("id"), "basis": basis}
    else:
        for m in BIB_ENTRY.finditer(turn.get("content") or ""):
            by_num[int(m.group(1))] = {"text": m.group(3).strip(), "url": m.group(2), "fact_id": None,
                                       "basis": "bibliography"}

    by_url = {}
    for s in ev._as_list(turn.get("sources")):
        if not isinstance(s, dict) or not s.get("url"):
            continue
        text = " ".join(x for x in (s.get("title", ""), s.get("content", "")) if x).strip()
        if text:
            by_url[ev.norm_url(s["url"])] = {"text": text, "url": s["url"]}

    # [Sn] -> the preview text the writer was shown, as printed in the bibliography;
    # the session source with the same URL when the bibliography has no quote.
    by_snip = {}
    for m in SNIP_BIB_ENTRY.finditer(turn.get("content") or ""):
        text = (m.group(3) or "").strip()
        if not text:
            text = (by_url.get(ev.norm_url(m.group(2))) or {}).get("text", "")
        if text:
            by_snip[int(m.group(1))] = {"text": text, "url": m.group(2)}
    if meta:
        for c in meta.get("snippet_citations") or []:
            n = int(c.get("n", 0))
            hit = by_url.get(ev.norm_url(c.get("url", "")))
            if n and n not in by_snip and hit:
                by_snip[n] = hit
    return by_num, by_url, by_snip


def build_items(turn, use_anchors=True, claims=True):
    """Judge items: one per cited sentence, with the union of its cited evidence.

    With use_anchors, a link to a page the Deep a ciclo read is judged on the
    passage Go anchored to the sentence ("page" excerpt), not on the preview.

    Sentences opened by Go's deduction label become "declared" items (V25-QA A5),
    never judged. With claims, every sentence that states something also gets
    "extra" evidence for the every-claim measure: what its paragraph cites and the
    two closest passages of what was read; sentences of 40+ characters without
    citations become "claim" items judged on that. Without claims they are
    "uncited" items, counted and listed only.
    """
    body = ev.answer_body(turn.get("content") or "")
    by_num, by_url, by_snip = evidence_maps(turn)
    by_anchor = anchor_map(turn) if use_anchors else {}
    loop = isinstance(turn.get("meta"), dict) and bool(turn["meta"].get("loop"))
    c_anchors = claim_anchors(turn) if claims else []
    c_pool = claim_pool(turn) if claims and not loop else []

    def resolve(text, nums, urls):
        excerpts, missing = [], []
        for n in nums:
            if n in by_num:
                excerpts.append({"ref": f"[{n}]", "kind": "fact", **by_num[n]})
            else:
                missing.append(f"[{n}]")
        for u in urls:
            anchored = None if u.startswith("[S") else by_anchor.get(ev.norm_url(u))
            if anchored:
                passage, basis = best_anchor(anchored, text)
                excerpts.append({"ref": u, "kind": "page", "text": passage, "url": u, "basis": basis})
                continue
            hit = by_snip.get(int(u[2:-1])) if u.startswith("[S") else by_url.get(ev.norm_url(u))
            if hit:
                excerpts.append({"ref": u, "kind": "snippet", **hit})
            else:
                missing.append(u)
        return excerpts, missing

    items = []
    prev = ""
    for block in paragraphs(body):
        sents = split_sentences(block)
        refs = [citations_in(x) for x in sents]
        for sent, (nums, urls, snips) in zip(sents, refs):
            urls = [u for u in urls if u] + [f"[S{n}]" for n in snips]
            text = clean_sentence(sent)
            extra = []
            if claims:
                p_nums = sorted({n for r in refs for n in r[0]} - set(nums))
                p_urls = sorted({u for r in refs for u in r[1] if u} - set(urls)) + \
                    [f"[S{n}]" for n in sorted({n for r in refs for n in r[2]} - set(snips))]
                extra, _ = resolve(text, p_nums[:3], p_urls[:3])
                extra += closest_evidence(text, c_anchors, c_pool)
            if DECLARED_LABEL.match(text):
                items.append({"sentence": text, "context": prev, "excerpts": [],
                              "missing": [], "kind": "declared"})
            elif (nums or urls) and len(text) >= 15:
                excerpts, missing = resolve(text, nums, urls)
                kind = "fact" if nums and not urls else ("snippet" if urls and not nums else "mixed")
                if kind == "snippet" and any(e["kind"] == "page" for e in excerpts):
                    kind = "page"
                items.append({"sentence": text, "context": prev, "excerpts": excerpts,
                              "missing": missing, "kind": kind, "extra": extra})
            elif not (nums or urls) and len(text) >= 40:
                items.append({"sentence": text, "context": prev, "excerpts": extra if claims else [],
                              "missing": [], "kind": "claim" if claims else "uncited"})
            if text:
                prev = text
    return items


# ---------------------------------------------------------------- judge calls

def load_env_file(path):
    vals = {}
    if path and os.path.exists(path):
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                vals[k.strip()] = v.strip().strip('"').strip("'")
    return vals


def judge_config(env_path, vedetta_base):
    keys = ("JUDGE_BASE_URL", "JUDGE_MODEL", "JUDGE_API_KEY", "JUDGE_REASONING_EFFORT")
    if os.environ.get("JUDGE_BASE_URL") and os.environ.get("JUDGE_MODEL"):
        cfg, source = {k: os.environ.get(k, "") for k in keys}, "environment"
    else:
        cfg, source = load_env_file(env_path), env_path
    if cfg.get("JUDGE_BASE_URL") and cfg.get("JUDGE_MODEL"):
        return {"base_url": cfg["JUDGE_BASE_URL"].rstrip("/"), "model": cfg["JUDGE_MODEL"],
                "api_key": cfg.get("JUDGE_API_KEY", ""), "effort": cfg.get("JUDGE_REASONING_EFFORT") or "low",
                "source": source}
    conf = ev.http_json(vedetta_base.rstrip("/") + "/api/config")
    eps = conf.get("llm_endpoints") or []
    default_id = conf.get("default_llm_endpoint") or "default"
    ep = next((e for e in eps if e.get("id") == default_id), eps[0] if eps else None)
    base = (ep or {}).get("base_url") or conf.get("llm_base_url")
    model = (ep or {}).get("model") or conf.get("llm_model")
    if not base or not model:
        raise SystemExit("no judge configured: create scripts/judge.env (see scripts/README.md)")
    return {"base_url": base.rstrip("/"), "model": model, "api_key": "", "effort": "none",
            "source": "vedetta default endpoint (same model family as the writer)"}


def parse_verdict(text):
    t = (text or "").upper()
    for v in ("UNSUPPORTED", "PARTIAL", "SUPPORTED"):  # UNSUPPORTED contains SUPPORTED
        if re.search(r"\b" + v + r"\b", t):
            return v
    return "ERROR"


def user_prompt(item):
    parts = []
    if item["context"]:
        parts.append(f"CONTEXT: {item['context']}")
    parts.append(f"SENTENCE: {item['sentence']}")
    for i, e in enumerate(item["excerpts"], 1):
        parts.append(f"EXCERPT {i} ({e['kind']}): {e['text']}")
    return "\n".join(parts)


def make_ask(cfg, timeout=120, system=None, build=None, max_tokens=1024):
    """A judge call for one item; system and build (item -> user message) default to the
    faithfulness question (the ruler passes its own, V25-QB)."""
    system = system or SYSTEM_PROMPT
    build = build or user_prompt

    def ask(item):
        payload = {
            "model": cfg["model"],
            "messages": [{"role": "system", "content": system},
                         {"role": "user", "content": build(item)}],
            "temperature": 0,
            "max_tokens": max_tokens,
            "stream": False,
        }
        if cfg.get("effort"):
            payload["reasoning_effort"] = cfg["effort"]
        headers = {"Content-Type": "application/json"}
        if cfg.get("api_key"):
            headers["Authorization"] = "Bearer " + cfg["api_key"]
        req = urllib.request.Request(cfg["base_url"] + "/chat/completions",
                                     data=json.dumps(payload).encode(), headers=headers)
        last = ""
        for attempt in range(4):
            try:
                with urllib.request.urlopen(req, timeout=timeout) as r:
                    d = json.loads(r.read().decode("utf-8"))
                return (d["choices"][0]["message"].get("content") or "").strip()
            except urllib.error.HTTPError as e:
                last = f"HTTP {e.code}"
                if e.code not in (429, 500, 502, 503, 504):
                    break
            except Exception as e:  # network errors, timeouts
                last = str(e)
            time.sleep(2 ** attempt)
        return "ERROR: " + last
    return ask


def judge_items(items, ask, workers=4):
    judgeable = [it for it in items if it["excerpts"]]
    with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        answers = list(pool.map(ask, judgeable))
    for it, ans in zip(judgeable, answers):
        it["raw"] = ans
        it["verdict"] = parse_verdict(ans)
    for it in items:
        if it["kind"] == "uncited":
            it["verdict"] = "UNCITED"
        elif it["kind"] == "claim" and not it["excerpts"]:
            it["verdict"] = "NOTHING_CLOSE"  # no passage of what was read shares its words
        elif it["kind"] == "declared":
            it["verdict"] = "DECLARED"
        elif not it["excerpts"]:
            it["verdict"] = "NO_EVIDENCE"  # cites something we cannot resolve
    # every claim: a cited sentence its own evidence does not fully support is judged
    # again with what its paragraph cites and the closest passages of what was read
    again = [it for it in items if it.get("extra") and it.get("verdict") in ("PARTIAL", "UNSUPPORTED", "NO_EVIDENCE")]
    with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        answers = list(pool.map(lambda it: ask({**it, "excerpts": it["excerpts"] + it["extra"]}), again))
    for it, ans in zip(again, answers):
        it["claim_raw"] = ans
        it["claim_verdict"] = parse_verdict(ans)
    for it in items:
        if "claim_verdict" not in it:
            v = it.get("verdict")
            it["claim_verdict"] = "NOTHING_CLOSE" if v == "NO_EVIDENCE" else v
    return items


# ---------------------------------------------------------------- report

def summarize(items):
    def counts(sel):
        c = {v: 0 for v in VERDICTS + ("ERROR", "NO_EVIDENCE", "UNCITED", "DECLARED", "NOTHING_CLOSE")}
        for it in sel:
            c[it["verdict"]] += 1
        judged = sum(c[v] for v in VERDICTS)
        c["judged"] = judged
        c["supported_pct"] = round(100 * c["SUPPORTED"] / judged, 1) if judged else None
        return c
    cited = [i for i in items if i["kind"] != "claim"]
    every = counts([{**i, "verdict": i.get("claim_verdict", i["verdict"])}
                    for i in items if i["kind"] not in ("uncited", "declared")])
    # every claim: cited and uncited sentences; one with nothing close in what was read
    # counts as not supported
    judged = every["judged"] + every["NOTHING_CLOSE"]
    every["judged"] = judged
    every["supported_pct"] = round(100 * every["SUPPORTED"] / judged, 1) if judged else None
    return {"all": counts(cited),
            "every_claim": every,
            "claim": counts([i for i in items if i["kind"] == "claim"]),
            "fact": counts([i for i in items if i["kind"] == "fact"]),
            "snippet": counts([i for i in items if i["kind"] in ("snippet", "mixed")]),
            "page": counts([i for i in items if i["kind"] == "page"])}


def evidence_basis(items):
    """How many cited fact excerpts were passages, aligned evidence or bare snippets:
    results are comparable only between runs with the same basis."""
    counts = {}
    for it in items:
        for e in it["excerpts"]:
            if e.get("kind") in ("fact", "page"):
                key = e.get("basis") if e["kind"] == "fact" else "page:" + str(e.get("basis"))
                counts[key] = counts.get(key, 0) + 1
    return ", ".join(f"{k}={v}" for k, v in sorted(counts.items(), key=lambda kv: str(kv[0])))


def render(session_id, title, cfg, items, summary):
    a = summary["all"]
    e = summary.get("every_claim") or {}
    cl = summary.get("claim") or {}
    lines = ["=" * 68,
             f"FAITHFULNESS JUDGE  session {session_id}",
             f"title: {(title or '')[:70]}",
             f"judge: {cfg['model']} @ {re.sub(r'^https?://', '', cfg['base_url'])} "
             f"(effort={cfg['effort'] or '-'}, from {os.path.basename(cfg['source']) if os.path.sep in cfg['source'] else cfg['source']})",
             "-" * 68,
             f"cited sentences: {len([i for i in items if i['kind'] not in ('claim', 'uncited', 'declared')])}  judged: {a['judged']}  "
             f"SUPPORTED {a['SUPPORTED']}  PARTIAL {a['PARTIAL']}  UNSUPPORTED {a['UNSUPPORTED']}  "
             f"(errors {a['ERROR']}, unresolved citations {a['NO_EVIDENCE']})",
             f"% SUPPORTED: {a['supported_pct'] if a['supported_pct'] is not None else 'n/a'}"
             f"   [facts: {summary['fact']['supported_pct']}  snippets/mixed: {summary['snippet']['supported_pct']}"
             f"  pages read: {summary['page']['supported_pct']}]",
             f"uncited claim-like sentences (not judged): {a['UNCITED']}",
             f"EVERY CLAIM (cited + uncited, on what was read): judged {e.get('judged', 0)}  SUPPORTED {e.get('SUPPORTED', 0)}  "
             f"% SUPPORTED: {e.get('supported_pct') if e.get('supported_pct') is not None else 'n/a'}"
             f"   [uncited: judged {cl.get('judged', 0)}, SUPPORTED {cl.get('SUPPORTED', 0)}, "
             f"PARTIAL {cl.get('PARTIAL', 0)}, UNSUPPORTED {cl.get('UNSUPPORTED', 0)}, nothing close {cl.get('NOTHING_CLOSE', 0)}]",
             f"declared deductions (not judged): {a['DECLARED']}",
             f"fact/page evidence shown to the judge: {evidence_basis(items) or 'none'}"]
    for verdict in ("UNSUPPORTED", "PARTIAL", "NO_EVIDENCE", "ERROR", "NOTHING_CLOSE", "UNCITED", "DECLARED"):
        sel = [i for i in items if i["verdict"] == verdict and (i["kind"] != "claim" or verdict == "NOTHING_CLOSE")]
        if not sel:
            continue
        lines.append("-" * 68)
        label = {"UNCITED": "UNCITED (no citation, not judged)",
                 "DECLARED": "DECLARED (deduction declared by the writer, not judged)"}.get(verdict, verdict)
        lines.append(f"{label} ({len(sel)}) — review by hand:")
        for i in sel:
            refs = " ".join(e["ref"] for e in i["excerpts"]) or " ".join(i["missing"])
            lines.append(f"  • {i['sentence'][:200]}")
            if verdict in ("UNCITED", "DECLARED"):
                continue
            lines.append(f"      cites {refs[:120]}")
            for e in i["excerpts"][:2]:
                lines.append(f"      evidence: «{e['text'][:200]}»")
            if verdict == "ERROR":
                lines.append(f"      judge said: {i.get('raw', '')[:120]}")
    weak = [i for i in items if i["kind"] == "claim" and i["verdict"] in ("UNSUPPORTED", "PARTIAL")]
    if weak:
        lines.append("-" * 68)
        lines.append(f"UNCITED CLAIMS NOT FULLY SUPPORTED by the closest passage ({len(weak)}) — review by hand:")
        for i in weak:
            lines.append(f"  • [{i['verdict']}] {i['sentence'][:200]}")
            lines.append(f"      closest: «{i['excerpts'][0]['text'][:200]}»  ({i['excerpts'][0].get('url', '')[:80]})")
    lines.append("=" * 68)
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description="Vedetta faithfulness judge (V25-T1.3)")
    ap.add_argument("sessions", nargs="+", help="session ids")
    ap.add_argument("--base-url", default=ev.DEFAULT_BASE)
    ap.add_argument("--env", default=DEFAULT_ENV)
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--out", default=DEFAULT_OUT)
    ap.add_argument("--evidence", choices=("anchor", "preview"), default="anchor")
    ap.add_argument("--claims", choices=("on", "off"), default="on")
    args = ap.parse_args()

    cfg = judge_config(args.env, args.base_url)
    ask = make_ask(cfg)
    os.makedirs(args.out, exist_ok=True)

    for sid in args.sessions:
        session = ev.fetch_session(args.base_url, sid)
        msgs = session.get("messages") or []
        turn = msgs[0] if msgs else {}
        items = judge_items(build_items(turn, use_anchors=args.evidence == "anchor", claims=args.claims == "on"),
                            ask, args.workers)
        summary = summarize(items)
        print(render(sid, session.get("title"), cfg, items, summary))
        record = {"session": sid, "judge": {k: cfg[k] for k in ("base_url", "model", "effort")}, "evidence": args.evidence, "claims": args.claims,
                  "judged_at": time.strftime("%Y-%m-%dT%H:%M:%S"), "summary": summary, "items": items}
        with open(os.path.join(args.out, f"judge_{sid}.json"), "w") as f:
            json.dump(record, f, ensure_ascii=False, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
