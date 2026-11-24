#!/usr/bin/env python3
"""Summary and verdict of benchmark rounds (V25-R2.4). Stdlib only, no network.

Reads the files written by eval.py, judge.py and bench.py in scripts/out:
  runs_TAG.json, eval_TAG.log, judge_TAG_<session>.txt,
  meta_TAG.json (profile and mode, V25-M1.3), server_TAG.log (provider usage, V25-M1.4)

A TAG names one round when runs_TAG.json exists, otherwise all rounds
runs_TAG_1.json, runs_TAG_2.json, ... (as written by bench.py). TAG+OTHER joins
the rounds of two tags one by one (R0+R0n: R0_1 with R0n_1, ...), for cases that
were run apart (V25-QB D6).

A round checks the cases its meta file lists; older rounds, without that list,
ran the 17 cases of suite core17. When rounds and baseline ran different cases,
the verdict compares only the cases they have in common.

Usage:
  python3 scripts/bench_report.py TAG [TAG ...] [--baseline TAG [TAG ...]] [--suite NAME]

Decision rule (docs/SPEC_R2_TRE_LIVELLI_DI_TEST.md §2.3): compare the pooled
% SUPPORTED of the rounds (SUPPORTED / judged, summed over all cases) with the
pooled % of the baseline. A change counts only if it is at least 4 points and
every round differs from the baseline in the same direction. A round with judge
errors or missing cases is not valid.
"""
import argparse
import glob
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_OUT = os.path.join(HERE, "out")
DEFAULT_BENCHMARK = os.path.join(HERE, "benchmark.json")
THRESHOLD = 4.0

JUDGE_RE = re.compile(
    r"judged: (\d+)\s+SUPPORTED (\d+)\s+PARTIAL (\d+)\s+UNSUPPORTED (\d+)\s+\(errors (\d+)"
)
UNCITED_RE = re.compile(r"uncited claim-like sentences \(not judged\): (\d+)")
# PIANO_DEEP_A_CICLO §4: every claim, cited or not, judged on what was read (judge.py --claims on).
EVERY_RE = re.compile(r"EVERY CLAIM .*?: judged (\d+)\s+SUPPORTED (\d+)")
# V25-QA A5: sentences the writer declared as its own reasoning; above this share of
# the answer sentences the writer is probably labelling everything (spec D5).
DECLARED_RE = re.compile(r"declared deductions \(not judged\): (\d+)")
DECLARED_MAX_SHARE = 0.2
# One search per tree node; every hop fuses the node lists, so the sum of
# "lists" is the number of searches (it matched the Brave dashboard in cloud1).
# The "query=" line is only written for non-general facets, so it undercounts.
RRF_RE = re.compile(r"\[search\] rrf lists=(\d+)")
SEARCH_ERR_RE = re.compile(r"\[composite\] chain provider \[\d+\] (\S+) returned error")
SERVED_RE = re.compile(r"\[reader\] session=\S+ url=\S+ (?:served_by=(\S+)|read via (\S+))")
TELEMETRY_RE = re.compile(r"\[telemetry\] session=\S+ providers_used=.* fallback=(true|false)(?: fallback_reason=(\".*\"))?")
PLANNER_FB_RE = re.compile(r"planner fallback activated")
# V25-T7.1b: stages that ran without the model in one session.
LLM_TROUBLE_RE = re.compile(r"\[telemetry\] session=\S+ llm_trouble=(\S+)")
EXA_RE = re.compile(r"\[exa\] session=\S* kind=(search|contents) .*cost=\$([\d.]+)")
# Fallback reasons grouped by kind, first match wins (texts from internal/reader and internal/search composite).
REASON_KINDS = [
    ("search error", re.compile(r"search: .*degradato|chain degradata")),
    ("PDF", re.compile(r"cannot parse PDF")),
    ("block page", re.compile(r"returned block page")),
    ("short page", re.compile(r"low-density SPA")),
    ("reader error", re.compile(r"reader \S+ failed")),
]
CACHE_RE = re.compile(
    r"cache: (\S+) - searches (?:\d+% hit \((\d+)/(\d+)\)|n/a), pages (?:\d+% hit \((\d+)/(\d+)\)|n/a)"
)


def pct(num, den):
    return 100.0 * num / den if den else None


def fmt_pct(v):
    return "n/a" if v is None else f"{v:.1f}"


def resolve_rounds(tag, out_dir):
    """Round names for a tag: the tag itself, or TAG_1, TAG_2, ... in numeric order.
    For TAG+OTHER each name is "TAG_k+OTHER_k"."""
    if "+" in tag:
        parts = [resolve_rounds(t, out_dir) for t in tag.split("+")]
        if not all(parts) or len({len(p) for p in parts}) != 1:
            return []
        return ["+".join(names) for names in zip(*parts)]
    if os.path.exists(os.path.join(out_dir, f"runs_{tag}.json")):
        return [tag]
    found = []
    for path in glob.glob(os.path.join(out_dir, f"runs_{glob.escape(tag)}_*.json")):
        suffix = os.path.basename(path)[len(f"runs_{tag}_"):-len(".json")]
        if suffix.isdigit():
            found.append((int(suffix), f"{tag}_{suffix}"))
    return [name for _, name in sorted(found)]


def parse_judge(text):
    m = JUDGE_RE.search(text)
    if not m:
        return None
    judged, sup, part, uns, err = map(int, m.groups())
    count = lambda rx: int(x[1]) if (x := rx.search(text)) else 0  # noqa: E731
    every = EVERY_RE.search(text)
    return {"judged": judged, "supported": sup, "partial": part, "unsupported": uns, "errors": err,
            "uncited": count(UNCITED_RE), "declared": count(DECLARED_RE),
            "every_judged": int(every[1]) if every else 0, "every_supported": int(every[2]) if every else 0}


def parse_cache(text):
    """Last run-level cache line of an eval log (the log may hold several reports)."""
    matches = CACHE_RE.findall(text)
    if not matches:
        return None
    mode, sh, st, ph, pt = matches[-1]
    num = lambda s: int(s) if s else 0  # noqa: E731
    return {"mode": mode, "search_hits": num(sh), "searches": num(st), "page_hits": num(ph), "pages": num(pt)}


def parse_usage(text):
    """Provider usage of one round from the bench instance server log (V25-M1.4)."""
    u = {"searches": 0, "search_errors": {}, "pages": {}, "sessions": 0, "fallback_sessions": 0,
         "reasons": {}, "planner_fallbacks": 0, "exa": {}, "llm_trouble_sessions": 0, "llm_trouble": {}}
    for line in text.splitlines():
        if m := RRF_RE.search(line):
            u["searches"] += int(m[1])
        elif m := SEARCH_ERR_RE.search(line):
            u["search_errors"][m[1]] = u["search_errors"].get(m[1], 0) + 1
        elif m := SERVED_RE.search(line):
            name = m[1] or m[2]
            u["pages"][name] = u["pages"].get(name, 0) + 1
        elif m := TELEMETRY_RE.search(line):
            u["sessions"] += 1
            if m[1] == "true":
                u["fallback_sessions"] += 1
                reason = json.loads(m[2]) if m[2] else ""
                kinds = [k for k, rx in REASON_KINDS if rx.search(reason)] or ["unknown"]
                for k in kinds:
                    u["reasons"][k] = u["reasons"].get(k, 0) + 1
        elif m := LLM_TROUBLE_RE.search(line):
            u["llm_trouble_sessions"] += 1
            for part in m[1].split(","):
                stage = part.split(":")[0]
                u["llm_trouble"][stage] = u["llm_trouble"].get(stage, 0) + 1
        elif PLANNER_FB_RE.search(line):
            u["planner_fallbacks"] += 1
        elif m := EXA_RE.search(line):
            calls, cost = u["exa"].get(m[1], (0, 0.0))
            u["exa"][m[1]] = (calls + 1, cost + float(m[2]))
    return u


def counts_text(d):
    return ", ".join(f"{k} {v}" for k, v in sorted(d.items(), key=lambda kv: (-kv[1], kv[0]))) or "none"


def usage_text(u):
    return (f"searches {u['searches']} (errors: {counts_text(u['search_errors'])})  "
            f"pages by reader: {counts_text(u['pages'])}  "
            f"fallback sessions {u['fallback_sessions']}/{u['sessions']} ({counts_text(u['reasons'])})  "
            f"planner fallbacks {u['planner_fallbacks']}  "
            f"llm trouble sessions {u.get('llm_trouble_sessions', 0)}/{u['sessions']}"
            + (f" ({counts_text(u['llm_trouble'])})" if u.get("llm_trouble") else "")
            + exa_text(u.get("exa")))


def exa_text(exa):
    """Exa spend of the round, summed from the cost Exa returns with every call (SPEC_EXA EXA.4)."""
    if not exa:
        return ""
    total = sum(cost for _, cost in exa.values())
    parts = ", ".join(f"{kind} {calls} calls ${cost:.3f}" for kind, (calls, cost) in sorted(exa.items()))
    return f"  exa ${total:.3f} ({parts})"


_LEGACY = None


def load_ruler(parts, out_dir):
    """Understanding counts of a round (ruler_NAME.json, V25-QB), joined over its parts;
    None when the ruler did not run on every part."""
    import ruler  # noqa: E402  (same directory)
    per_case, completeness = {}, None
    for part in parts:
        rec = read_json(os.path.join(out_dir, f"ruler_{part}.json"), None)
        if rec is None:
            return None
        per_case.update(rec.get("understanding") or {})
        if "completeness" in rec:
            completeness = (completeness or {}) | rec["completeness"]
    return {"per_case": per_case, "totals": ruler.totals(per_case),
            "completeness": ruler.completeness_totals(completeness) if completeness is not None else None}


def legacy_cases():
    """The 17 cases rounds ran before V25-QB (suite core17)."""
    global _LEGACY
    if _LEGACY is None:
        with open(DEFAULT_BENCHMARK) as f:
            _LEGACY = list(json.load(f).get("suites", {}).get("core17", []))
    return _LEGACY


def round_expected(meta, expected_cases):
    """The cases a round should have: the list in its meta, else (older rounds run on the
    whole benchmark) the 17 cases of core17."""
    if meta.get("cases"):
        return list(meta["cases"])
    if expected_cases:
        legacy = legacy_cases()
        if legacy and set(legacy) < set(expected_cases):
            return legacy
    return expected_cases


def read_json(path, default):
    if not os.path.exists(path):
        return default
    with open(path) as f:
        return json.load(f)


def load_round(name, out_dir, expected_cases):
    """Totals of one round, plus the reasons why it is not valid (empty when valid).
    A name "A+B" joins rounds A and B (cases run apart)."""
    parts = name.split("+")
    runs, owner, meta, expected = {}, {}, {}, []
    for part in parts:
        part_runs = read_json(os.path.join(out_dir, f"runs_{part}.json"), None)
        if part_runs is None:
            raise FileNotFoundError(f"runs_{part}.json")
        part_meta = read_json(os.path.join(out_dir, f"meta_{part}.json"), {})
        meta = meta or part_meta
        for case, sid in part_runs.items():
            runs[case], owner[case] = sid, part
        if len(parts) == 1:
            expected = round_expected(part_meta, expected_cases)
        else:
            expected += part_meta.get("cases") or list(part_runs)
    r = {"name": name, "cases": len(runs), "judged": 0, "supported": 0, "partial": 0,
         "unsupported": 0, "errors": 0, "uncited": 0, "declared": 0, "every_judged": 0, "every_supported": 0,
         "problems": [], "per_case": {},
         "case_counts": {}}
    missing = sorted(set(expected) - set(runs)) if expected else []
    if missing:
        r["problems"].append(f"missing cases: {', '.join(missing)}")
    for case, sid in runs.items():
        path = os.path.join(out_dir, f"judge_{owner[case]}_{sid}.txt")
        if not os.path.exists(path):
            r["problems"].append(f"no judge file for {case}")
            continue
        with open(path) as f:
            j = parse_judge(f.read())
        if j is None:
            r["problems"].append(f"judge file without totals for {case}")
            continue
        for k in COUNT_KEYS:
            r[k] += j[k]
        r["per_case"][case] = pct(j["supported"], j["judged"])
        r["case_counts"][case] = j
    if r["errors"]:
        r["problems"].append(f"judge errors: {r['errors']}")
    r["pct"] = pct(r["supported"], r["judged"])
    r["every_pct"] = pct(r["every_supported"], r["every_judged"])
    log = os.path.join(out_dir, f"eval_{parts[0]}.log")
    r["cache"] = None
    if os.path.exists(log):
        with open(log) as f:
            r["cache"] = parse_cache(f.read())
    r["meta"] = meta
    r["ruler"] = load_ruler(parts, out_dir)
    r["usage"] = None
    server_log = os.path.join(out_dir, f"server_{parts[0]}.log")
    if os.path.exists(server_log):
        with open(server_log, errors="replace") as f:
            r["usage"] = parse_usage(f.read())
    return r


COUNT_KEYS = ("judged", "supported", "partial", "unsupported", "errors", "uncited", "declared",
              "every_judged", "every_supported")


def pooled(rounds):
    return pct(sum(r["supported"] for r in rounds), sum(r["judged"] for r in rounds))


def pooled_every(rounds):
    return pct(sum(r.get("every_supported", 0) for r in rounds), sum(r.get("every_judged", 0) for r in rounds))


def restrict(r, cases):
    """A copy of round r counting only the given cases."""
    out = dict(r)
    for k in COUNT_KEYS:
        out[k] = sum(c.get(k, 0) for case, c in r["case_counts"].items() if case in cases)
    out["pct"] = pct(out["supported"], out["judged"])
    out["every_pct"] = pct(out["every_supported"], out["every_judged"])
    return out


def common_cases(rounds):
    sets = [set(r["case_counts"]) for r in rounds]
    return set.intersection(*sets) if sets else set()


def verdict(rounds, base_pooled, key="pct", pool=pooled):
    """Italian verdict label and the pooled difference (spec §2.3)."""
    if any(r["problems"] for r in rounds):
        return "giro non valido", None
    diff = pool(rounds) - base_pooled
    per_round = [r[key] - base_pooled for r in rounds]
    if abs(diff) >= THRESHOLD and all((d > 0) == (diff > 0) and d != 0 for d in per_round):
        return ("migliora" if diff > 0 else "peggiora"), diff
    return "nessun effetto misurabile", diff


def cache_text(c):
    if not c:
        return "cache n/a"
    s = f"{100 * c['search_hits'] / c['searches']:.0f}% ({c['search_hits']}/{c['searches']})" if c["searches"] else "n/a"
    p = f"{100 * c['page_hits'] / c['pages']:.0f}% ({c['page_hits']}/{c['pages']})" if c["pages"] else "n/a"
    return f"cache {c['mode']}: searches {s}, pages {p}"


def mode_text(r):
    """Profile and mode of a round; live rounds start from an empty cache, so no hit rate."""
    meta = r.get("meta") or {}
    profile = meta.get("profile", "home")
    if meta.get("mode") == "live":
        return f"profile {profile}, live"
    return f"profile {profile}, {cache_text(r['cache'])}"


def declared_text(r):
    """Uncited and declared sentences of a round (V25-QA A5), with the declared share."""
    uncited, declared = r.get("uncited", 0), r.get("declared", 0)
    total = r["judged"] + uncited + declared
    share = declared / total if total else 0.0
    text = f"uncited {uncited}, declared deductions {declared} ({100 * share:.0f}% of sentences)"
    if share > DECLARED_MAX_SHARE:
        text += f" — over {100 * DECLARED_MAX_SHARE:.0f}%: the writer may be labelling everything"
    return text


def round_line(r):
    line = (f"  {r['name']:<22} cases {r['cases']:>2}  judged {r['judged']:>4}  "
            f"SUPPORTED {r['supported']:>4}  PARTIAL {r['partial']:>3}  UNSUPPORTED {r['unsupported']:>3}  "
            f"% {fmt_pct(r['pct']):>5}  | {mode_text(r)}")
    line += f"\n      sentences not judged: {declared_text(r)}"
    if r.get("every_judged"):
        line += (f"\n      every claim (cited + uncited, on what was read): judged {r['every_judged']}  "
                 f"SUPPORTED {r['every_supported']}  % {fmt_pct(r.get('every_pct'))}")
    if r.get("ruler"):
        import ruler  # noqa: E402
        line += f"\n      {ruler.totals_line(r['ruler']['totals'])}"
        if r["ruler"].get("completeness"):
            line += f"\n      {ruler.completeness_line(r['ruler']['completeness'])}"
    if r.get("usage"):
        line += f"\n      usage: {usage_text(r['usage'])}"
    for p in r["problems"]:
        line += f"\n      NOT VALID: {p}"
    return line


def load_group(tags, out_dir, expected_cases):
    rounds = []
    for tag in tags:
        names = resolve_rounds(tag, out_dir)
        if not names:
            sys.exit(f"no runs file for tag {tag!r} in {out_dir}")
        rounds.extend(load_round(n, out_dir, expected_cases) for n in names)
    return rounds


def identity_lines(rounds):
    """What differs between the rounds compared: environment switches and configuration
    fields recorded in their meta (Piano dopo audit 2.1 / M8). Rounds that did not record
    them are named apart."""
    known = [r for r in rounds if "switches" in (r.get("meta") or {})]
    lines = []
    for section in ("switches", "config"):
        keys = sorted({k for r in known for k in (r["meta"].get(section) or {})})
        for k in keys:
            values = []
            for r in known:
                v = (r["meta"].get(section) or {}).get(k)
                values.append(v if isinstance(v, str) else json.dumps(v))
            if len(set(values)) > 1:
                lines.append(f"differs {k}: " + ", ".join(f"{r['name']}={v}" for r, v in zip(known, values)))
    unknown = [r["name"] for r in rounds if r not in known]
    if unknown and known:
        lines.append(f"switches not recorded: {' '.join(unknown)}")
    return lines


def report(tags, baseline, out_dir, expected_cases):
    rounds = load_group(tags, out_dir, expected_cases)
    lines = [f"rounds: {' '.join(tags)}"]
    if baseline:
        lines += identity_lines(rounds + load_group(baseline, out_dir, expected_cases))
    elif len(rounds) > 1:
        lines += identity_lines(rounds)
    lines += [round_line(r) for r in rounds]
    lines.append(f"  pooled % SUPPORTED: {fmt_pct(pooled(rounds))}")
    if any(r.get("every_judged") for r in rounds):
        lines.append(f"  pooled % SUPPORTED, every claim: {fmt_pct(pooled_every(rounds))}")
    if baseline:
        base = load_group(baseline, out_dir, expected_cases)
        lines.append(f"baseline: {' '.join(baseline)}")
        lines += [round_line(r) for r in base]
        base_pooled = pooled(base)
        lines.append(f"  pooled % SUPPORTED: {fmt_pct(base_pooled)}")
        if any(r["problems"] for r in base):
            lines.append("verdict: baseline non valida")
        else:
            common = common_cases(rounds + base)
            if any(set(r["case_counts"]) != common for r in rounds + base):
                rounds = [restrict(r, common) for r in rounds]
                base = [restrict(r, common) for r in base]
                base_pooled = pooled(base)
                lines.append(f"compared on the {len(common)} cases in common: rounds {fmt_pct(pooled(rounds))}, "
                             f"baseline {fmt_pct(base_pooled)}")
            v, diff = verdict(rounds, base_pooled)
            per_round = ", ".join(f"{r['pct'] - base_pooled:+.1f}" for r in rounds if r["pct"] is not None)
            d = "" if diff is None else f" (difference {diff:+.1f} points; per round {per_round}; threshold {THRESHOLD:.0f})"
            lines.append(f"verdict: {v}{d}")
            if all(r.get("every_judged") for r in rounds + base):
                base_every = pooled_every(base)
                v, diff = verdict(rounds, base_every, key="every_pct", pool=pooled_every)
                per_round = ", ".join(f"{r['every_pct'] - base_every:+.1f}" for r in rounds if r.get("every_pct") is not None)
                d = "" if diff is None else f" (difference {diff:+.1f} points; per round {per_round}; threshold {THRESHOLD:.0f})"
                lines.append(f"verdict, every claim: {v}{d}")
    return "\n".join(lines)


def expected_from_benchmark(path, suite):
    with open(path) as f:
        bench = json.load(f)
    if suite:
        cases = bench.get("suites", {}).get(suite)
        if not cases:
            sys.exit(f"unknown suite: {suite}")
        return list(cases)
    return [c["id"] for c in bench.get("cases", [])]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("tags", nargs="+", help="round tags (runs_TAG.json) or round groups (runs_TAG_N.json)")
    ap.add_argument("--baseline", nargs="+", help="baseline tags, same rules")
    ap.add_argument("--suite", help="the rounds ran this suite (default: all benchmark cases)")
    ap.add_argument("--out", default=DEFAULT_OUT, help="directory with runs/eval/judge files")
    ap.add_argument("--benchmark", default=DEFAULT_BENCHMARK)
    args = ap.parse_args()
    expected = expected_from_benchmark(args.benchmark, args.suite)
    print(report(args.tags, args.baseline, args.out, expected))


if __name__ == "__main__":
    main()
