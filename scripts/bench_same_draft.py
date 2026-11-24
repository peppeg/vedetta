#!/usr/bin/env python3
"""Two review methods from one draft: the same research, then each method on its own.

The first arm runs the research live through llm_replay_proxy.py, which saves it. Every
later arm gets the same answers from the proxy and the same pages from a copy of the
first arm's cache, so it reaches the same first draft. Only what comes after the
send-back is asked to the model again.

Arms, for tag T and --arms whole,lines,lines,whole, are the rounds T_w1, T_l1, T_l2, T_w2.
It checks at the end that every sent-back answer of every arm had the first arm's draft.

Usage:
  python3 scripts/bench_same_draft.py --tag sd1 [--arms whole,lines,lines,whole] [--cases a,b]
Do not use the Vedetta UI meanwhile: the instance shares the LLM. No judge is called here.
"""
import argparse
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
OUT = os.path.join(HERE, "out")
INSTANCE = os.path.join(HERE, "bench_instance.sh")
PROXY = os.path.join(HERE, "llm_replay_proxy.py")
BENCH = os.environ.get("VEDETTA_BENCH_DIR", os.path.expanduser("~/vedetta-bench"))
BASE_URL = "http://localhost:" + os.environ.get("VEDETTA_BENCH_PORT", "3031")
PROXY_PORT = 3099
PROFILES = {"whole": "home-deep-loop-linkcheck", "lines": "home-deep-loop-lines"}


def log(msg):
    print(f"[same-draft {time.strftime('%H:%M:%S')}] {msg}", flush=True)


def instance(*args):
    subprocess.run([INSTANCE, *args], check=True, stdout=subprocess.DEVNULL)


def through_proxy():
    """Points the prepared instance at the proxy and returns the real address. Prints no secret."""
    path = os.path.join(BENCH, "data", "config.local")
    with open(path) as f:
        cfg = json.load(f)
    endpoints = cfg.get("llm_endpoints") or []
    default = next((ep for ep in endpoints if ep.get("id") == cfg.get("llm_default_id")), None)
    upstream = default["base_url"] if default else cfg["llm_base_url"]
    mine = f"http://127.0.0.1:{PROXY_PORT}/v1"
    cfg["llm_base_url"] = mine
    for ep in endpoints:
        if ep.get("base_url") == upstream:
            ep["base_url"] = mine
    with open(path, "w") as f:
        json.dump(cfg, f, indent=2)
    env = os.path.join(BENCH, ".env")
    with open(env) as f:
        lines = [ln for ln in f if not re.match(r"\s*LLM_BASE_URL\s*=", ln)]
    with open(env, "w") as f:
        f.writelines(lines)
        f.write(f"LLM_BASE_URL={mine}\n")
    return upstream


def run_arm(name, method, seed, record_dir, benchmark):
    runs_path = os.path.join(OUT, f"runs_{name}.json")
    instance("prepare", "--profile", PROFILES[method], "--live", name)
    if seed:
        src, dst = os.path.join(BENCH, "data", f"cache-live-{seed}"), os.path.join(BENCH, "data", f"cache-live-{name}")
        shutil.rmtree(dst)
        shutil.copytree(src, dst)
    upstream = through_proxy()
    with open(os.path.join(BENCH, "commit.txt")) as f:
        commit = f.read().strip()
    with open(os.path.join(OUT, f"meta_{name}.json"), "w") as f:
        json.dump({"profile": PROFILES[method], "mode": "live", "commit": commit, "answer_mode": "deep",
                   "same_draft": {"seed": seed or name, "method": method}}, f, indent=2)
    proxy_log = os.path.join(OUT, f"proxy_{name}.log")
    proxy = subprocess.Popen([sys.executable, PROXY, "--upstream", upstream, "--dir", record_dir, "--mode",
                              "replay" if seed else "record", "--port", str(PROXY_PORT), "--log", proxy_log],
                             stdout=subprocess.DEVNULL)
    try:
        time.sleep(1)
        instance("start")
        log(f"{name}: {method}, {'from the draft of ' + seed if seed else 'recording the research'}")
        cmd = [sys.executable, os.path.join(HERE, "eval.py"), "--all", "--base-url", BASE_URL, "--mode", "deep",
               "--log", os.path.join(OUT, f"eval_{name}.log"), "--runs-out", runs_path]
        if benchmark:
            cmd += ["--benchmark", benchmark]
        with open(os.path.join(OUT, f"eval_{name}.stdout"), "w") as out:
            subprocess.run(cmd, check=True, stdout=out, stderr=subprocess.STDOUT)
        log(f"{name}: done")
    finally:
        instance("stop")
        proxy.terminate()
        proxy.wait(timeout=20)
        bench_log = os.path.join(BENCH, "vedetta.log")
        if os.path.exists(bench_log):
            shutil.copyfile(bench_log, os.path.join(OUT, f"server_{name}.log"))


def loop_of(conn, sid):
    row = conn.execute("select content, meta_json from messages where session_id=? and role='assistant' "
                       "order by created_at limit 1", (sid,)).fetchone()
    if not row:
        return None, {}
    return row[0], (json.loads(row[1] or "{}").get("loop") or {})


def check_drafts(names):
    """Every arm must have reached the first arm's draft, or the comparison means nothing."""
    conn = sqlite3.connect(f"file:{os.path.join(BENCH, 'data', 'vedetta.db')}?mode=ro", uri=True)
    conn.text_factory = lambda b: b.decode("utf-8", "replace")
    seed = json.load(open(os.path.join(OUT, f"runs_{names[0]}.json")))
    for name in names:
        with open(os.path.join(OUT, f"proxy_{name}.log")) as f:
            kinds = [ln.split()[1] for ln in f if len(ln.split()) == 4]
        counts = {k: kinds.count(k) for k in sorted(set(kinds))}
        log(f"{name}: requests {counts}")
    for name in names[1:]:
        runs = json.load(open(os.path.join(OUT, f"runs_{name}.json")))
        same, differ, lost = 0, [], []
        for case, sid in seed.items():
            if case not in runs:
                lost.append(case)
                continue
            _, a = loop_of(conn, sid)
            _, b = loop_of(conn, runs[case])
            if a.get("draft") and b.get("draft"):
                (differ.append(case) if a["draft"] != b["draft"] else None)
                same += a["draft"] == b["draft"]
        log(f"{name}: same draft {same}, different {differ}, not run {lost}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--tag", required=True)
    ap.add_argument("--arms", default="whole,lines,lines,whole")
    ap.add_argument("--cases", help="only these benchmark cases, comma-separated")
    args = ap.parse_args()
    arms = args.arms.split(",")
    if any(a not in PROFILES for a in arms):
        sys.exit("arms are whole or lines")
    seen, names = {}, []
    for a in arms:
        seen[a] = seen.get(a, 0) + 1
        names.append(f"{args.tag}_{a[0]}{seen[a]}")
    if any(os.path.exists(os.path.join(OUT, f"runs_{n}.json")) for n in names):
        sys.exit(f"rounds already exist for tag {args.tag}")
    benchmark = None
    if args.cases:
        with open(os.path.join(HERE, "benchmark.json")) as f:
            bench = json.load(f)
        wanted = args.cases.split(",")
        bench["cases"] = [c for c in bench["cases"] if c["id"] in wanted]
        benchmark = os.path.join(OUT, f"benchmark_{args.tag}.json")
        with open(benchmark, "w") as f:
            json.dump(bench, f)
    record_dir = os.path.join(BENCH, "data", f"llm-replay-{args.tag}")
    for i, (name, method) in enumerate(zip(names, arms)):
        run_arm(name, method, names[0] if i else None, record_dir, benchmark)
    check_drafts(names)


if __name__ == "__main__":
    main()
