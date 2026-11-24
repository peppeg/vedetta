#!/usr/bin/env python3
"""Unit tests for scripts/bench.py: the measures of Deep rounds (Piano dopo audit 5.1). Stdlib only, no network.

Run: python3 -m unittest scripts/test_bench.py
"""
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import bench  # noqa: E402

PAID = ("judge.py", "judge_pages.py", "judge_links.py", "score_known.py", "ruler.py")
FREE_FLAGS = ("--estimate", "--fetch-only", "--dry-run", "--no-completeness")


def script(step):
    return os.path.basename(step["cmd"][1]) if step.get("cmd") else None


class DeepSteps(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        d = self.tmp.name
        self.out = os.path.join(d, "out")
        self.bench = os.path.join(d, "bench")
        os.makedirs(self.out)
        for name in ("t_1", "t_2"):
            os.makedirs(os.path.join(self.bench, "data", f"cache-live-{name}"))
            with open(os.path.join(self.out, f"runs_{name}.json"), "w") as f:
                json.dump({"plain": f"sid-{name}-p", "with-known": f"sid-{name}-k"}, f)
        self.benchmark = os.path.join(d, "benchmark.json")
        with open(self.benchmark, "w") as f:
            json.dump({"cases": [{"id": "plain"}], "extra_cases": [{"id": "with-known", "known": "entry"}]}, f)

    def tearDown(self):
        self.tmp.cleanup()

    def steps(self, judge, names=("t_1", "t_2"), baseline=None):
        return bench.deep_steps(list(names), judge, baseline, self.out, self.bench, self.benchmark)

    def test_an_estimate_calls_no_judge(self):
        steps = self.steps(judge=False)
        for s in steps:
            if script(s) in PAID:
                self.assertTrue(any(flag in s["cmd"] for flag in FREE_FLAGS), s["cmd"])
        self.assertNotIn("ruler.py", [script(s) for s in steps])
        self.assertNotIn("judge.py", [script(s) for s in steps])

    def test_a_judging_runs_the_four_measures_and_the_ruler(self):
        steps = self.steps(judge=True)
        self.assertEqual([script(s) for s in steps],
                         ["free_report.py", "judge_pages.py", "judge_links.py", "judge_links.py", "score_known.py", "ruler.py"])
        for s in steps:
            self.assertFalse(any(flag in s["cmd"] for flag in FREE_FLAGS), s["cmd"])

    def test_every_measure_reads_the_same_rounds_database_and_cache(self):
        db = os.path.join(self.bench, "data", "vedetta.db")
        for judge in (False, True):
            for s in self.steps(judge):
                self.assertIn(db, s["cmd"], s["cmd"])
        links = [s for s in self.steps(True) if script(s) == "judge_links.py"]
        self.assertEqual([s["cmd"][2] for s in links], ["t_1", "t_2"])
        self.assertIn(os.path.join(self.bench, "data", "cache-live-t_1"), links[0]["cmd"])
        self.assertIn(os.path.join(self.out, "runs_t_1.json"), links[0]["cmd"])
        pages = next(s for s in self.steps(True) if script(s) == "judge_pages.py")
        self.assertEqual(pages["cmd"][2:4], ["t_1", "t_2"])

    def test_the_baseline_goes_to_the_judge_on_the_pages_read(self):
        pages = next(s for s in self.steps(True, baseline=["base"]) if script(s) == "judge_pages.py")
        self.assertEqual(pages["cmd"][pages["cmd"].index("--baseline") + 1], "base")

    def test_a_round_without_its_cache_gets_the_free_report_and_a_note(self):
        with open(os.path.join(self.out, "runs_old_1.json"), "w") as f:
            json.dump({"plain": "sid-old"}, f)
        steps = self.steps(judge=True, names=("old_1",))
        self.assertEqual([script(s) for s in steps if s.get("cmd")], ["free_report.py", "ruler.py"])
        notes = [s["note"] for s in steps if s.get("note")]
        self.assertEqual(len(notes), 1)
        self.assertIn("old_1", notes[0])

    def test_links_already_judged_are_not_paid_twice(self):
        saved = os.path.join(self.out, "judgelinks_t_1.json")
        with open(saved, "w") as f:
            json.dump({"summary": {}, "summary_one_link": {}, "summary_sentences": {}}, f)
        steps = self.steps(judge=True)
        self.assertEqual([s["cmd"][2] for s in steps if script(s) == "judge_links.py"], ["t_2"])
        self.assertEqual([s["saved"] for s in steps if s.get("saved")], [saved])

    def test_known_answers_only_when_a_case_has_one(self):
        for name in ("t_1", "t_2"):
            with open(os.path.join(self.out, f"runs_{name}.json"), "w") as f:
                json.dump({"plain": "sid"}, f)
        self.assertNotIn("score_known.py", [script(s) for s in self.steps(judge=True)])


class Report(unittest.TestCase):
    def test_one_report_with_a_section_per_measure(self):
        results = [("free report", 0, "official page  3/4"), ("judge on the pages read", 0, "pooled % SUPPORTED: 88.0"),
                   ("known answers", 2, "Traceback")]
        text = bench.deep_report_text("t", ["t_1", "t_2"], True, results)
        self.assertIn("t_1 t_2", text.splitlines()[0])
        self.assertIn("judged", text.splitlines()[0])
        for title, _, body in results:
            self.assertIn(title, text)
            self.assertIn(body, text)
        self.assertIn("FAILED (exit 2)", text)
        self.assertIn("estimate", bench.deep_report_text("t", ["t_1"], False, results).splitlines()[0])


class Main(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.out = self.tmp.name
        self.rounds, self.measures, self.reports = [], [], []

        def run_round(name, corpus, suite, profile=None, live=False, cases=None, answer_mode=None, judge=True):
            self.rounds.append({"name": name, "judge": judge, "mode": answer_mode})
            with open(os.path.join(self.out, f"runs_{name}.json"), "w") as f:
                json.dump({"plain": "sid"}, f)

        def measure_deep(tag, names, judge, baseline=None):
            self.measures.append({"tag": tag, "names": list(names), "judge": judge, "baseline": baseline})
            return "report"

        def report(tags, baseline, out_dir, expected):
            self.reports.append(list(tags))
            return "old report"

        self.patches = [mock.patch.object(bench, "OUT", self.out), mock.patch.object(bench, "run_round", run_round),
                        mock.patch.object(bench, "measure_deep", measure_deep), mock.patch.object(bench.br, "report", report)]
        for p in self.patches:
            p.start()

    def tearDown(self):
        for p in self.patches:
            p.stop()
        self.tmp.cleanup()

    def main(self, *argv):
        with mock.patch.object(sys, "argv", ["bench.py", *argv]), mock.patch("builtins.print"):
            bench.main()

    def test_deep_rounds_get_the_deep_measures_not_the_nearest_passage_judge(self):
        self.main("--tag", "t", "--live", "--suite", "reduced", "--mode", "deep", "--baseline", "base")
        self.assertEqual([(r["name"], r["judge"]) for r in self.rounds], [("t_1", False), ("t_2", False)])
        self.assertEqual(self.measures, [{"tag": "t", "names": ["t_1", "t_2"], "judge": True, "baseline": ["base"]}])
        self.assertEqual(self.reports, [])

    def test_deep_rounds_without_judge_get_the_estimate(self):
        self.main("--tag", "t", "--live", "--suite", "reduced", "--mode", "deep", "--no-judge", "--rounds", "1")
        self.assertEqual([r["judge"] for r in self.rounds], [False])
        self.assertEqual([(m["names"], m["judge"]) for m in self.measures], [(["t_1"], False)])

    def test_measure_only_runs_no_round(self):
        for name in ("t_1", "t_2", "t_3"):
            with open(os.path.join(self.out, f"runs_{name}.json"), "w") as f:
                json.dump({"plain": "sid"}, f)
        self.main("--tag", "t", "--measure-only")
        self.assertEqual(self.rounds, [])
        self.assertEqual(self.measures, [{"tag": "t", "names": ["t_1", "t_2", "t_3"], "judge": True, "baseline": None}])
        self.main("--tag", "t_2", "--measure-only", "--no-judge")
        self.assertEqual((self.measures[-1]["names"], self.measures[-1]["judge"]), (["t_2"], False))

    def test_measure_only_stops_when_the_rounds_are_not_there(self):
        with self.assertRaises(SystemExit):
            self.main("--tag", "nothing", "--measure-only")
        self.assertEqual(self.measures, [])

    def test_without_mode_the_rounds_are_deep_too(self):
        # Map R1 (17/11/2026): one way only, so a round without --mode is a Deep round.
        self.main("--tag", "t", "--live", "--suite", "reduced", "--rounds", "1")
        self.assertEqual([(r["judge"], r["mode"]) for r in self.rounds], [(False, "deep")])
        self.assertEqual(self.measures, [{"tag": "t", "names": ["t_1"], "judge": True, "baseline": None}])
        self.assertEqual(self.reports, [])


if __name__ == "__main__":
    unittest.main()
