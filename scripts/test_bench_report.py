#!/usr/bin/env python3
"""Unit tests for scripts/bench_report.py (V25-R2.4). Stdlib only, no network.

Run: python3 -m unittest scripts/test_bench_report.py
"""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import bench_report as br  # noqa: E402

CASES = ["a", "b"]


def judge_text(judged, sup, part, uns, err=0):
    return (
        "====\nFAITHFULNESS JUDGE  session x\n----\n"
        f"cited sentences: {judged}  judged: {judged}  SUPPORTED {sup}  PARTIAL {part}  UNSUPPORTED {uns}  "
        f"(errors {err}, unresolved citations 0)\n% SUPPORTED: 0\n"
    )


class Fixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.out = self.tmp.name

    def tearDown(self):
        self.tmp.cleanup()

    def write_round(self, name, per_case, err=0, cache_line=None, skip_judge=()):
        """per_case: {case: (judged, supported)}; the rest of judged is PARTIAL."""
        runs = {c: f"sid-{name}-{c}" for c in per_case}
        with open(os.path.join(self.out, f"runs_{name}.json"), "w") as f:
            json.dump(runs, f)
        for c, (judged, sup) in per_case.items():
            if c in skip_judge:
                continue
            with open(os.path.join(self.out, f"judge_{name}_{runs[c]}.txt"), "w") as f:
                f.write(judge_text(judged, sup, judged - sup, 0, err))
        if cache_line:
            with open(os.path.join(self.out, f"eval_{name}.log"), "w") as f:
                f.write(cache_line)


class TestParsing(Fixture):
    def test_parse_judge(self):
        j = br.parse_judge(judge_text(24, 20, 3, 1, 2))
        self.assertEqual(j, {"judged": 24, "supported": 20, "partial": 3, "unsupported": 1, "errors": 2,
                             "uncited": 0, "declared": 0, "every_judged": 0, "every_supported": 0})
        self.assertIsNone(br.parse_judge("no totals here"))

    def test_parse_judge_every_claim(self):
        text = judge_text(24, 20, 3, 1, 2) + ("\nEVERY CLAIM (cited + uncited, on what was read): judged 90  SUPPORTED 54  "
                                              "% SUPPORTED: 60.0   [uncited: judged 55, SUPPORTED 23]\n")
        j = br.parse_judge(text)
        self.assertEqual((j["every_judged"], j["every_supported"]), (90, 54))

    def test_parse_judge_uncited_and_declared(self):
        text = (judge_text(24, 20, 3, 1, 0) + "\nuncited claim-like sentences (not judged): 5"
                "\ndeclared deductions (not judged): 2\n")
        j = br.parse_judge(text)
        self.assertEqual((j["uncited"], j["declared"]), (5, 2))

    def test_declared_share_warning(self):
        r = {"judged": 20, "uncited": 2, "declared": 3}
        self.assertEqual(br.declared_text(r), "uncited 2, declared deductions 3 (12% of sentences)")
        r["declared"] = 10
        self.assertIn("the writer may be labelling everything", br.declared_text(r))

    def test_parse_cache_takes_last_run_line(self):
        text = (
            "cache: mode=replay searches=4/4 pages=6/7\n"
            "cache: replay - searches 50% hit (1/2), pages 50% hit (1/2)\n"
            "cache: replay - searches 63% hit (33/52), pages 80% hit (80/100)\n"
        )
        self.assertEqual(br.parse_cache(text),
                         {"mode": "replay", "search_hits": 33, "searches": 52, "page_hits": 80, "pages": 100})

    def test_parse_cache_na(self):
        c = br.parse_cache("cache: record - searches n/a, pages 39% hit (39/99)\n")
        self.assertEqual((c["searches"], c["page_hits"], c["pages"]), (0, 39, 99))
        self.assertIsNone(br.parse_cache("nothing"))

    def test_resolve_rounds(self):
        for name in ("x_1", "x_2", "x_10", "x_base", "xy_1"):
            self.write_round(name, {"a": (1, 1)})
        self.assertEqual(br.resolve_rounds("x", self.out), ["x_1", "x_2", "x_10"])
        self.assertEqual(br.resolve_rounds("x_2", self.out), ["x_2"])
        self.assertEqual(br.resolve_rounds("missing", self.out), [])


class TestRounds(Fixture):
    def test_totals_and_pooled(self):
        self.write_round("r_1", {"a": (10, 8), "b": (10, 6)})
        self.write_round("r_2", {"a": (20, 20), "b": (0, 0)})
        rounds = br.load_group(["r"], self.out, CASES)
        self.assertEqual([r["pct"] for r in rounds], [70.0, 100.0])
        self.assertAlmostEqual(br.pooled(rounds), 34 / 40 * 100)
        self.assertEqual(rounds[0]["partial"], 6)
        self.assertFalse(any(r["problems"] for r in rounds))

    def test_invalid_rounds(self):
        self.write_round("miss", {"a": (10, 8)})
        self.write_round("err", {"a": (10, 8), "b": (10, 8)}, err=1)
        self.write_round("nojudge", {"a": (10, 8), "b": (10, 8)}, skip_judge=("b",))
        for name, word in (("miss", "missing cases: b"), ("err", "judge errors"), ("nojudge", "no judge file for b")):
            r = br.load_round(name, self.out, CASES)
            self.assertTrue(any(word in p for p in r["problems"]), (name, r["problems"]))

    def test_cache_read_from_eval_log(self):
        self.write_round("c", {"a": (1, 1), "b": (1, 1)},
                         cache_line="cache: replay - searches 83% hit (43/52), pages 92% hit (90/98)\n")
        r = br.load_round("c", self.out, CASES)
        self.assertEqual(r["cache"]["page_hits"], 90)
        self.assertIn("searches 83% (43/52)", br.round_line(r))


def rnd(pct_value, problems=()):
    return {"supported": pct_value, "judged": 100, "pct": float(pct_value), "problems": list(problems)}


class TestVerdict(unittest.TestCase):
    def test_improves_and_worsens(self):
        self.assertEqual(br.verdict([rnd(84), rnd(83)], 78.0)[0], "migliora")
        self.assertEqual(br.verdict([rnd(72), rnd(73)], 78.0)[0], "peggiora")

    def test_threshold_is_four_points(self):
        self.assertEqual(br.verdict([rnd(82), rnd(82)], 78.0)[0], "migliora")
        self.assertEqual(br.verdict([rnd(81), rnd(82)], 78.0)[0], "nessun effetto misurabile")

    def test_rounds_must_agree_in_direction(self):
        # pooled +5, but the second round is below the baseline
        self.assertEqual(br.verdict([rnd(88), rnd(77)], 78.0)[0], "nessun effetto misurabile")

    def test_invalid_round_wins(self):
        v, diff = br.verdict([rnd(90), rnd(90, ["judge errors: 1"])], 78.0)
        self.assertEqual((v, diff), ("giro non valido", None))


class TestReport(Fixture):
    def test_report_with_baseline(self):
        self.write_round("base_1", {"a": (50, 39), "b": (50, 39)})
        self.write_round("base_2", {"a": (50, 39), "b": (50, 39)})
        self.write_round("new_1", {"a": (50, 42), "b": (50, 42)})
        self.write_round("new_2", {"a": (50, 43), "b": (50, 42)})
        text = br.report(["new"], ["base"], self.out, CASES)
        self.assertIn("pooled % SUPPORTED: 78.0", text)
        self.assertIn("verdict: migliora", text)

    def test_invalid_baseline(self):
        self.write_round("base", {"a": (50, 39)})
        self.write_round("new", {"a": (50, 42), "b": (50, 42)})
        self.assertIn("verdict: baseline non valida", br.report(["new"], ["base"], self.out, CASES))


class TestCaseLists(Fixture):
    """V25-QB D6: the benchmark grew from 17 to 19 cases."""

    def write_meta(self, name, cases):
        with open(os.path.join(self.out, f"meta_{name}.json"), "w") as f:
            json.dump({"profile": "home", "mode": "live", "cases": cases}, f)

    def test_meta_cases_decide(self):
        self.write_round("r", {"a": (10, 8)})
        self.write_meta("r", ["a"])
        self.assertEqual(br.load_round("r", self.out, ["a", "b", "c"])["problems"], [])

    def test_old_round_falls_back_to_core17(self):
        self.write_round("old", {"a": (10, 8), "b": (10, 8)})
        saved = br._LEGACY
        br._LEGACY = ["a", "b"]
        try:
            self.assertEqual(br.load_round("old", self.out, ["a", "b", "c"])["problems"], [])
        finally:
            br._LEGACY = saved

    def test_joined_rounds(self):
        for k in (1, 2):
            self.write_round(f"R0_{k}", {"a": (10, 8)})
            self.write_meta(f"R0_{k}", ["a"])
            self.write_round(f"R0n_{k}", {"c": (10, 5)})
            self.write_meta(f"R0n_{k}", ["c"])
        self.assertEqual(br.resolve_rounds("R0+R0n", self.out), ["R0_1+R0n_1", "R0_2+R0n_2"])
        r = br.load_round("R0_1+R0n_1", self.out, ["a", "c"])
        self.assertEqual((r["cases"], r["judged"], r["supported"], r["problems"]), (2, 20, 13, []))

    def test_verdict_on_common_cases(self):
        for k in (1, 2):
            self.write_round(f"new_{k}", {"a": (50, 45), "c": (50, 10)})
            self.write_meta(f"new_{k}", ["a", "c"])
            self.write_round(f"base_{k}", {"a": (50, 40)})
            self.write_meta(f"base_{k}", ["a"])
        text = br.report(["new"], ["base"], self.out, ["a", "c"])
        self.assertIn("compared on the 1 cases in common: rounds 90.0, baseline 80.0", text)
        self.assertIn("verdict: migliora (difference +10.0", text)


if __name__ == "__main__":
    unittest.main()


SERVER_LOG = """\
2026/10/20 10:00:01 [search] session=s1 facet=authoritative lang=en query="suez canal workers"
2026/10/20 10:00:02 [search] rrf lists=3 docs=29 k=60
2026/10/20 10:00:02 [search] rrf lists=2 docs=12 k=60
2026/10/20 10:00:03 [composite] chain provider [0] brave returned error: status 429
2026/10/20 10:00:04 [search] session=s1 facet=news lang=it query="canale di suez"
2026/10/20 10:00:05 [reader] session=s1 url=https://a.example/x served_by=http chars=900
2026/10/20 10:00:06 [reader] session=s1 url=https://b.example/y served_by=firecrawl chars=4000
2026/10/20 10:00:07 [reader] session=s1 url=https://pubmed.ncbi.nlm.nih.gov/1/ read via europepmc (2697 chars)
2026/10/20 10:00:08 [reader] session=s1 url=https://c.example/z reader=http block_page marker="captcha" chars=120
2026/10/20 10:00:09 [deep] planner fallback activated: json parse failed: invalid character 'I'
2026/10/20 10:00:10 [telemetry] session=s1 providers_used=search:brave,reader:http engines=[brave:29] sources_fetched=29 (pdf=0, html=4, spa=0) fallback=true fallback_reason="search: brave degradato (status 429); reader: reader http failed (status 403); falling back to firecrawl"
2026/10/20 10:00:11 [telemetry] session=s2 providers_used=search:brave,reader:http engines=[brave:20] sources_fetched=20 (pdf=0, html=4, spa=0) fallback=false
2026/10/20 10:00:12 [telemetry] session=s3 providers_used=search:brave,reader:http engines=[brave:20] sources_fetched=20 (pdf=0, html=4, spa=0) fallback=true
2026/10/20 10:00:13 [telemetry] doc url=https://a.example/x format=html chars=900
"""


class TestUsage(Fixture):
    def test_parse_usage(self):
        u = br.parse_usage(SERVER_LOG)
        self.assertEqual(u["searches"], 5)
        self.assertEqual(u["search_errors"], {"brave": 1})
        self.assertEqual(u["pages"], {"http": 1, "firecrawl": 1, "europepmc": 1})
        self.assertEqual((u["sessions"], u["fallback_sessions"]), (3, 2))
        # one session with two kinds of reason, one older log line without a reason
        self.assertEqual(u["reasons"], {"search error": 1, "reader error": 1, "unknown": 1})
        self.assertEqual(u["planner_fallbacks"], 1)

    def test_usage_and_profile_in_report(self):
        self.write_round("c_1", {"a": (10, 9), "b": (10, 8)},
                         cache_line="cache: record - searches 0% hit (0/2), pages 0% hit (0/3)\n")
        with open(os.path.join(self.out, "meta_c_1.json"), "w") as f:
            json.dump({"profile": "cloud", "mode": "live", "commit": "abc"}, f)
        with open(os.path.join(self.out, "server_c_1.log"), "w") as f:
            f.write(SERVER_LOG)
        text = br.report(["c"], None, self.out, CASES)
        self.assertIn("profile cloud, live", text)
        self.assertNotIn("cache record", text)
        self.assertIn("pages by reader: europepmc 1, firecrawl 1, http 1", text)
        self.assertIn("fallback sessions 2/3", text)
        self.assertIn("planner fallbacks 1", text)

    def test_exa_spend(self):
        log = ("2026/10/20 13:00:00 [exa] session=s1 kind=search results=10 cost=$0.0070\n"
               "2026/10/20 13:00:01 [exa] session=s1 kind=search results=9 cost=$0.0070\n"
               "2026/10/20 13:00:02 [exa] session=s1 kind=contents url=https://x.it/a source=crawled chars=900 cost=$0.0010\n"
               "2026/10/20 13:00:03 [exa] session=s1 kind=contents url=https://x.it/b status=error cost=$0.0000\n"
               "2026/10/20 13:00:03 [reader] session=s1 url=https://x.it/a served_by=exa chars=900\n")
        u = br.parse_usage(log)
        self.assertEqual(u["exa"], {"search": (2, 0.014), "contents": (2, 0.001)})
        self.assertEqual(u["pages"], {"exa": 1})
        self.assertIn("exa $0.015 (contents 2 calls $0.001, search 2 calls $0.014)", br.usage_text(u))
        self.assertNotIn("exa", br.usage_text(br.parse_usage(SERVER_LOG)))

    def test_round_without_meta_is_home_replay(self):
        self.write_round("r_1", {"a": (10, 9), "b": (10, 8)},
                         cache_line="cache: replay - searches 50% hit (1/2), pages 50% hit (1/2)\n")
        text = br.report(["r"], None, self.out, CASES)
        self.assertIn("profile home, cache replay: searches 50% (1/2)", text)
        self.assertNotIn("usage:", text)


class TestRoundIdentity(Fixture):
    """Piano dopo audit 2.1 / M8: a round says what it is, and the report names what differs."""

    def write_meta(self, name, switches=None, config=None):
        meta = {"profile": "home", "mode": "live", "commit": "abc1234", "cases": CASES}
        if switches is not None:
            meta["switches"] = switches
        if config is not None:
            meta["config"] = config
        with open(os.path.join(self.out, f"meta_{name}.json"), "w") as f:
            json.dump(meta, f)

    def test_report_names_different_switches(self):
        for name, rerank in (("on", "true"), ("off", "false")):
            self.write_round(name, {"a": (10, 8), "b": (10, 8)})
            self.write_meta(name, {"VEDETTA_DEEP_LOOP": "true", "VEDETTA_DEEP_LOOP_RERANK": rerank},
                            {"llm_model": "qwen"})
        out = br.report(["on"], ["off"], self.out, CASES)
        head = out.split("\n")[:4]
        self.assertTrue(any("VEDETTA_DEEP_LOOP_RERANK" in l and "on=true" in l and "off=false" in l for l in head), out)
        self.assertNotIn("VEDETTA_DEEP_LOOP:", out)  # same in both: not named

    def test_report_names_different_config_and_unrecorded(self):
        self.write_round("new", {"a": (10, 8), "b": (10, 8)})
        self.write_meta("new", {"VEDETTA_DEEP_LOOP": "true"}, {"llm_model": "qwen"})
        self.write_round("other", {"a": (10, 8), "b": (10, 8)})
        self.write_meta("other", {"VEDETTA_DEEP_LOOP": "true"}, {"llm_model": "deepseek"})
        self.write_round("old", {"a": (10, 8), "b": (10, 8)})
        self.write_meta("old")
        out = br.report(["new", "other"], ["old"], self.out, CASES)
        self.assertIn("llm_model: new=qwen, other=deepseek", out)
        self.assertIn("switches not recorded: old", out)

    def test_write_meta_records_switches_never_keys(self):
        import bench
        bench_dir = os.path.join(self.out, "bench")
        os.makedirs(os.path.join(bench_dir, "data"))
        with open(os.path.join(bench_dir, ".env"), "w") as f:
            f.write("LLM_API_KEY=sk-secret-1\nVEDETTA_DEEP_LOOP=true\nVEDETTA_EXA_API_KEY=sk-secret-2\n"
                    "VEDETTA_SETTINGS_CODE=sk-secret-3\nVEDETTA_ADMIN_PASSWORD=sk-secret-4\n# VEDETTA_OFF=1\n"
                    "VEDETTA_DEEP_LOOP_REVIEW = \"true\"\nVEDETTA_HF_TOKEN=sk-secret-5\n")
        with open(os.path.join(bench_dir, "data", "config.local"), "w") as f:
            json.dump({"llm_model": "qwen", "llm_api_key": "sk-secret-6", "exa_api_key": "sk-secret-7",
                       "llm_base_url": "http://user:sk-secret-8@host:8000/v1?key=sk-secret-9",
                       "reader_providers": ["http", "jina"], "llm_endpoints": [{"api_key": "sk-secret-10"}]}, f)
        with open(os.path.join(bench_dir, "commit.txt"), "w") as f:
            f.write("abc1234-dirty\n")
        old_out, old_env = bench.OUT, os.environ.get("VEDETTA_BENCH_DIR")
        bench.OUT, os.environ["VEDETTA_BENCH_DIR"] = self.out, bench_dir
        try:
            bench.write_meta("r1", None, True, None)
        finally:
            bench.OUT = old_out
            if old_env is None:
                del os.environ["VEDETTA_BENCH_DIR"]
            else:
                os.environ["VEDETTA_BENCH_DIR"] = old_env
        with open(os.path.join(self.out, "meta_r1.json")) as f:
            raw = f.read()
        self.assertNotIn("sk-secret", raw)
        meta = json.loads(raw)
        self.assertEqual(meta["commit"], "abc1234-dirty")
        self.assertEqual(meta["switches"], {"VEDETTA_DEEP_LOOP": "true", "VEDETTA_DEEP_LOOP_REVIEW": "true"})
        self.assertEqual(meta["config"], {"llm_model": "qwen", "llm_base_url": "http://host:8000/v1",
                                          "reader_providers": ["http", "jina"]})
