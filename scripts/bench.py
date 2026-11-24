#!/usr/bin/env python3
"""Benchmark rounds on the separate instance (V25-R2.3, live rounds and profiles V25-M1.3).

For every round NAME_1, NAME_2, ...:
  1. bench_instance.sh prepare CORPUS   (fresh copy of the frozen corpus)
     or, with --live, prepare --live NAME (new empty cache in record mode)
     with --profile P also: the instance config gets scripts/profiles/P.json on top
  2. bench_instance.sh start            (port 3031; production untouched)
  3. eval.py --all on the instance      -> runs_NAME_N.json, eval_NAME_N.log, eval_NAME_N.stdout
     (and turns_NAME_N.json, the seconds of each turn, when the suite holds conversations)
  4. judge.py for every session         -> judge_NAME_N_<session>.txt
     ruler.py for the round                -> ruler_NAME_N.json (V25-QB)
  5. bench_instance.sh stop             (always, even after an error) -> server_NAME_N.log
Every round also gets meta_NAME_N.json (profile, mode, commit) for bench_report.py.
Then prints bench_report.py for the rounds (and the verdict, with --baseline).

Since map R1 (17/11/2026) every round is the Deep a ciclo: step 4 (judge.py, on the nearest
passage of the old pipeline's facts) no longer runs, and after the last round the rounds get the
measures made for the Deep, always with the same options, in one report (Piano dopo audit 5.1):
  free_report.py   no judge
  judge_pages.py   every sentence on the pages the answer read
  judge_links.py   every linked sentence on the page it links (a judging already saved is reused)
  score_known.py   the cases with a known answer
  ruler.py         completeness
The report goes to deepreport_TAG.txt. With --no-judge the judges only count what they would
send (the pages the answers link are fetched, no judge is called): deepestimate_TAG.txt.
--measure-only runs no round: the same measures on rounds that already ran, so the order is
rounds with --no-judge, the estimate, the go-ahead, then --measure-only.

Usage:
  python3 scripts/bench.py --tag NAME [--rounds 2] [--corpus DIR | --live] [--profile P] [--suite reduced] [--baseline TAG ...]
  python3 scripts/bench.py --tag NAME --measure-only [--no-judge] [--baseline TAG ...]
  --no-judge stops each round after the cases: steps 4 and the report cost money, run the judges you need by hand.

Do not use the Vedetta UI during a round: both instances share the LLM. See
docs/SPEC_R2_TRE_LIVELLI_DI_TEST.md.
"""
import argparse
import glob
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time
import urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
OUT = os.path.join(HERE, "out")
INSTANCE = os.path.join(HERE, "bench_instance.sh")
BASE_URL = "http://localhost:" + os.environ.get("VEDETTA_BENCH_PORT", "3031")

sys.path.insert(0, HERE)
import bench_report as br  # noqa: E402


def newest_corpus():
    found = sorted(glob.glob(os.path.join(REPO, "data", "cache-corpus-*")))
    if not found:
        sys.exit("no data/cache-corpus-* found; pass --corpus")
    return found[-1]


def instance(*args):
    subprocess.run([INSTANCE, *args], check=True)


def log(msg):
    print(f"[bench {time.strftime('%H:%M:%S')}] {msg}", flush=True)


# What a round is (Piano dopo audit 2.1 / M8): the VEDETTA_* switches of its .env, except
# names that may hold a secret, and a closed list of config.local fields (keys live there too).
SECRET_WORDS = ("KEY", "TOKEN", "SECRET", "PASS", "CODE")
CONFIG_FIELDS = ("llm_model", "llm_base_url", "llm_default_id", "reader_providers",
                 "search_providers", "default_search_provider", "paid_fallback", "search_fallback_on_empty",
                 "jina_url", "searxng_url", "exa_search_type", "deep_loop_review_lines", "deep_loop_push", "deep_loop_grant")


def round_switches(bench):
    out = {}
    try:
        with open(os.path.join(bench, ".env")) as f:
            lines = f.readlines()
    except OSError:
        return out
    for line in lines:
        m = re.match(r"\s*(VEDETTA_[A-Z0-9_]+)\s*=\s*(.*?)\s*$", line)
        if m and not any(w in m.group(1) for w in SECRET_WORDS):
            out[m.group(1)] = m.group(2).strip('"').strip("'")
    return out


def bare_url(v):
    """An address without user, password, query or fragment: those may carry a key."""
    if not isinstance(v, str) or "://" not in v:
        return v
    u = urllib.parse.urlsplit(v)
    host = u.hostname or ""
    if u.port:
        host += f":{u.port}"
    return urllib.parse.urlunsplit((u.scheme, host, u.path, "", ""))


def round_config(bench):
    try:
        with open(os.path.join(bench, "data", "config.local")) as f:
            cfg = json.load(f)
    except (OSError, ValueError):
        return {}
    return {k: bare_url(cfg[k]) for k in CONFIG_FIELDS if k in cfg}


def write_meta(name, profile, live, corpus, cases=None, answer_mode=None):
    bench = os.environ.get("VEDETTA_BENCH_DIR", os.path.expanduser("~/vedetta-bench"))
    commit = ""
    try:
        with open(os.path.join(bench, "commit.txt")) as f:
            commit = f.read().strip()
    except OSError:
        pass
    meta = {"profile": profile or "home", "mode": "live" if live else "replay", "commit": commit,
            "switches": round_switches(bench), "config": round_config(bench)}
    if not live:
        meta["corpus"] = corpus
    if answer_mode:
        meta["answer_mode"] = answer_mode  # every case run in this mode (PIANO_DEEP_A_CICLO M2)
    if cases:
        meta["cases"] = list(cases)  # what the round ran (V25-QB: the benchmark grew from 17 to 19)
    with open(os.path.join(OUT, f"meta_{name}.json"), "w") as f:
        json.dump(meta, f, indent=2)


def run_round(name, corpus, suite, profile=None, live=False, cases=None, answer_mode=None, judge=True):
    runs_path = os.path.join(OUT, f"runs_{name}.json")
    eval_cmd = [sys.executable, os.path.join(HERE, "eval.py"), "--all", "--base-url", BASE_URL,
                "--log", os.path.join(OUT, f"eval_{name}.log"), "--runs-out", runs_path,
                "--turns-out", os.path.join(OUT, f"turns_{name}.json")]  # written only when the round has conversations
    if suite:
        eval_cmd += ["--suite", suite]
    if answer_mode:
        eval_cmd += ["--mode", answer_mode]

    prepare = ["prepare"] + (["--profile", profile] if profile else [])
    prepare += ["--live", name] if live else [corpus]
    instance(*prepare)
    write_meta(name, profile, live, corpus, cases, answer_mode)
    try:
        instance("start")
        log(f"{name}: running the cases")
        with open(os.path.join(OUT, f"eval_{name}.stdout"), "w") as out:
            subprocess.run(eval_cmd, check=True, stdout=out, stderr=subprocess.STDOUT)
        with open(runs_path) as f:
            runs = json.load(f)
        if not judge:
            log(f"{name}: {len(runs)} cases done, not judged")
            return
        log(f"{name}: {len(runs)} cases done, judging")
        for case, sid in runs.items():
            path = os.path.join(OUT, f"judge_{name}_{sid}.txt")
            with open(path, "w") as out:
                rc = subprocess.run([sys.executable, os.path.join(HERE, "judge.py"), sid, "--base-url", BASE_URL],
                                    stdout=out, stderr=subprocess.STDOUT).returncode
            if rc != 0:
                log(f"{name}: judge failed on {case} (exit {rc}), see {path}")
        # the ruler reads the sessions from the bench database (V25-QB)
        rc = subprocess.run([sys.executable, os.path.join(HERE, "ruler.py"), name],
                            stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT).returncode
        if rc != 0:
            log(f"{name}: ruler failed (exit {rc})")
    finally:
        instance("stop")
        # keep the server log of the round ([read] lines show the pages actually read)
        bench_log = os.path.join(os.environ.get("VEDETTA_BENCH_DIR", os.path.expanduser("~/vedetta-bench")), "vedetta.log")
        if os.path.exists(bench_log):
            shutil.copyfile(bench_log, os.path.join(OUT, f"server_{name}.log"))


def bench_dir():
    return os.environ.get("VEDETTA_BENCH_DIR", os.path.expanduser("~/vedetta-bench"))


def has_known(names, out_dir, benchmark):
    """Whether a round ran a case with a known answer (score_known.py has something to score)."""
    with open(benchmark) as f:
        bench = json.load(f)
    known = {c["id"] for c in bench.get("cases", []) + bench.get("extra_cases", []) if c.get("known")}
    for name in names:
        with open(os.path.join(out_dir, f"runs_{name}.json")) as f:
            if known & set(json.load(f)):
                return True
    return False


def deep_steps(names, judge, baseline=None, out_dir=None, bench=None, benchmark=None):
    """The measures of Deep rounds, in order, always with the same options (Piano dopo audit 5.1).
    Each step has a title and a command to run, or a saved judging to print, or a note.
    Without judge every paid script only counts what it would send."""
    out_dir = out_dir or OUT
    bench = bench or bench_dir()
    db = os.path.join(bench, "data", "vedetta.db")
    count = [] if judge else ["--estimate"]

    def py(script, *args):
        return [sys.executable, os.path.join(HERE, script), *args]

    steps = [{"title": "free report", "cmd": py("free_report.py", *names, "--db", db, "--cache-root", os.path.join(bench, "data"))}]
    # the page judges need the pages the round read: only live rounds keep their cache
    with_pages = [n for n in names if os.path.isdir(os.path.join(bench, "data", f"cache-live-{n}"))]
    without = [n for n in names if n not in with_pages]
    if without:
        steps.append({"title": "rounds without their cache",
                      "note": f"{' '.join(without)}: the cache of the round is gone (a replay round), "
                              "so the judges on the pages read and on the linked pages are skipped"})
    if with_pages:
        steps.append({"title": "judge on the pages read (judge_pages)",
                      "cmd": py("judge_pages.py", *with_pages, *(["--baseline", *baseline] if baseline else []), "--db", db, *count)})
    for name in with_pages:
        title = f"judge on the linked page (judge_links), {name}"
        saved = os.path.join(out_dir, f"judgelinks_{name}.json")
        if os.path.exists(saved):
            steps.append({"title": title, "saved": saved})
            continue
        steps.append({"title": title,
                      "cmd": py("judge_links.py", name, "--db", db, "--cache", os.path.join(bench, "data", f"cache-live-{name}"),
                                "--runs", os.path.join(out_dir, f"runs_{name}.json"), *([] if judge else ["--fetch-only"]))})
    if has_known(names, out_dir, benchmark or br.DEFAULT_BENCHMARK):
        steps.append({"title": "known answers (score_known)", "cmd": py("score_known.py", "--tags", *names, "--db", db, *count)})
    if judge:
        steps.append({"title": "completeness (ruler)", "cmd": py("ruler.py", *names, "--db", db)})
    return steps


def deep_report_text(tag, names, judged, results):
    """One report: a section per measure, with what the measure printed."""
    how = "judged" if judged else "estimate only, no judge called"
    lines = [f"Deep measures of {' '.join(names)} (tag {tag}): {how}"]
    for title, rc, body in results:
        lines += ["", f"== {title} ==" + (f"  FAILED (exit {rc})" if rc else ""), body]
    return "\n".join(lines)


def rounds_identity(names):
    """What each round is, from its meta, and what differs between them."""
    rounds = [{"name": n, "meta": br.read_json(os.path.join(OUT, f"meta_{n}.json"), {})} for n in names]
    lines = []
    for r in rounds:
        m = r["meta"]
        lines.append(f"{r['name']}: profile {m.get('profile', '?')}, {m.get('mode', '?')}, commit {m.get('commit') or '?'}, "
                     f"{len(m.get('cases') or [])} cases" if m else f"{r['name']}: no meta file")
    return "\n".join(lines + br.identity_lines(rounds))


def measure_deep(tag, names, judge, baseline=None):
    results = [("rounds", 0, rounds_identity(names))]
    for step in deep_steps(names, judge, baseline):
        log(f"{tag}: {step['title']}")
        if step.get("cmd"):
            done = subprocess.run(step["cmd"], cwd=REPO, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors="replace")
            results.append((step["title"], done.returncode, done.stdout.strip("\n")))  # keep the first line's indent: tables
        elif step.get("saved"):
            import judge_links as jl  # noqa: E402
            with open(step["saved"]) as f:
                results.append((step["title"], 0, f"already judged, read from {step['saved']}\n" + jl.format_summary(json.load(f))))
        else:
            results.append((step["title"], 0, step["note"]))
    text = deep_report_text(tag, names, judge, results)
    path = os.path.join(OUT, f"{'deepreport' if judge else 'deepestimate'}_{tag}.txt")
    with open(path, "w") as f:
        f.write(text + "\n")
    print(text)
    log(f"{tag}: report saved in {path}")
    return text


def interrupt(signum, _frame):
    # Turn kill/hangup into Ctrl+C: subprocess.run then kills the running child
    # (eval.py or judge.py) and the finally block stops the instance.
    raise KeyboardInterrupt(f"signal {signum}")


def main():
    signal.signal(signal.SIGTERM, interrupt)
    signal.signal(signal.SIGHUP, interrupt)
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--tag", required=True, help="round names are TAG_1, TAG_2, ...")
    ap.add_argument("--rounds", type=int, default=2)
    ap.add_argument("--corpus", help="frozen cache corpus (default: newest data/cache-corpus-*)")
    ap.add_argument("--live", action="store_true", help="live rounds: no corpus, each round records into a new empty cache")
    ap.add_argument("--profile", help="instance profile from scripts/profiles (default: the home config.local)")
    ap.add_argument("--suite", help="run only this benchmark suite (e.g. reduced)")
    ap.add_argument("--mode", choices=("deep",), default="deep",
                    help="every case runs as the Deep a ciclo, the only way since map R1 (17/11/2026)")
    ap.add_argument("--baseline", nargs="+", help="baseline tags for the verdict (bench_report.py)")
    ap.add_argument("--force", action="store_true", help="overwrite rounds that already have a runs file")
    ap.add_argument("--no-judge", action="store_true",
                    help="no paid judge: the cases only, no report; for Deep rounds the free report and the judges' estimate")
    ap.add_argument("--measure-only", action="store_true",
                    help="run no round: the Deep measures on the rounds of --tag that already ran (with --no-judge, the estimate)")
    args = ap.parse_args()

    if args.measure_only:
        names = br.resolve_rounds(args.tag, OUT)
        if not names:
            sys.exit(f"no runs file for tag {args.tag!r} in {OUT}")
        measure_deep(args.tag, names, judge=not args.no_judge, baseline=args.baseline)
        return

    expected = br.expected_from_benchmark(br.DEFAULT_BENCHMARK, args.suite)  # exits on unknown suite
    if args.live and args.corpus:
        sys.exit("--live and --corpus exclude each other")
    if args.profile and not os.path.exists(os.path.join(HERE, "profiles", f"{args.profile}.json")):
        sys.exit(f"unknown profile: {args.profile}")
    corpus = None if args.live else (os.path.abspath(args.corpus) if args.corpus else newest_corpus())
    names = [f"{args.tag}_{i}" for i in range(1, args.rounds + 1)]
    existing = [n for n in names if os.path.exists(os.path.join(OUT, f"runs_{n}.json"))]
    if existing and not args.force:
        sys.exit(f"rounds already exist: {', '.join(existing)} (use another --tag or --force)")
    os.makedirs(OUT, exist_ok=True)

    source = "live (new empty cache per round)" if args.live else f"corpus {corpus}"
    log(f"tag {args.tag}, {args.rounds} rounds, profile {args.profile or 'home'}, {source}, suite {args.suite or 'all'}")
    deep = args.mode == "deep"  # judged after the rounds, by the measures made for the Deep
    for name in names:
        try:
            run_round(name, corpus, args.suite, args.profile, args.live, expected, args.mode, judge=not args.no_judge and not deep)
        except subprocess.CalledProcessError as e:
            log(f"{name}: stopped, command failed: {' '.join(map(str, e.cmd))} (exit {e.returncode})")
            # the round is incomplete; bench_report marks it not valid
    ready = [n for n in names if os.path.exists(os.path.join(OUT, f"runs_{n}.json"))]
    if not ready:
        sys.exit("no round produced a runs file")
    if deep:
        measure_deep(args.tag, ready, judge=not args.no_judge, baseline=args.baseline)
    elif not args.no_judge:
        print(br.report(ready, args.baseline, OUT, expected))


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt as e:
        sys.exit(f"interrupted ({e or 'Ctrl+C'}); instance stopped, the current round is incomplete")
