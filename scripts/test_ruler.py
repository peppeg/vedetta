#!/usr/bin/env python3
"""Unit tests for scripts/ruler.py (V25-QB). Stdlib only, no network.

Run: python3 -m unittest scripts/test_ruler.py
"""
import json
import os
import sqlite3
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ruler  # noqa: E402

SHEET = {
    "entities": [
        {"name": "Niri", "aliases": [], "home": ["github.com/YaLTeR/niri"]},
        {"name": "DankMaterialShell", "aliases": ["DMS"], "home": ["danklinux.com"]},
        {"name": "Caelestia Shell", "aliases": ["Caelestia"], "home": ["github.com/caelestia-dots"]},
    ],
    "contents": ["x"],
}

TURN = {
    "subqueries": [
        {"query": "Niri DankMaterialShell Fedora 44 install"},
        {"query": "Caelestia Shell themes"},
        {"query": "fedora niri così riesco a farmi un'idea grazie"},
    ],
    "facts": [
        {"claim": "DMS ships a settings panel", "source_url": "https://danklinux.com/docs/settings"},
        {"claim": "Niri is a scrollable tiling compositor", "source_url": "https://github.com/YaLTeR/niri/wiki"},
        {"claim": "Another Niri fact", "source_url": "https://github.com/YaLTeR/niri/wiki"},
        {"claim": "unrelated", "source_url": "https://github.com/someone/dotfiles"},
    ],
}


class UnderstandingTest(unittest.TestCase):
    def test_counts(self):
        u = ruler.understanding(TURN, SHEET)
        self.assertEqual(u["queries"], 3)
        self.assertEqual(u["joint"], ["Niri DankMaterialShell Fedora 44 install"])
        self.assertEqual(u["courtesy"], ["fedora niri così riesco a farmi un'idea grazie"])
        self.assertEqual(u["unread"], ["Caelestia Shell"])
        self.assertEqual(u["official"], {"Niri": 1, "DankMaterialShell": 1, "Caelestia Shell": 0})

    def test_whole_words_only(self):
        e = {"name": "PUN", "aliases": [], "home": []}
        self.assertTrue(ruler.mentions("il PUN di ottobre", e))
        self.assertFalse(ruler.mentions("punto di vista", e))
        self.assertFalse(ruler.COURTESY_RE.search("grazielle festival"))
        self.assertTrue(ruler.COURTESY_RE.search("Danke für die Hilfe"))

    def test_home_domain_and_prefix(self):
        e = {"home": ["berlin.de", "github.com/YaLTeR/niri"]}
        self.assertTrue(ruler.on_home("https://service.berlin.de/dienstleistung/120686/", e))
        self.assertTrue(ruler.on_home("https://www.berlin.de/", e))
        self.assertFalse(ruler.on_home("https://notberlin.de/", e))
        self.assertTrue(ruler.on_home("https://github.com/YaLTeR/niri/blob/main/README.md", e))
        self.assertFalse(ruler.on_home("https://github.com/YaLTeR/niri-fork", e))

    def test_totals_line(self):
        t = ruler.totals({"c": ruler.understanding(TURN, SHEET)})
        self.assertEqual(ruler.totals_line(t), "understanding: joint 1/3, courtesy 1/3, "
                                               "entities without facts 1/3, official pages with facts 2")

    def test_json_strings_from_db(self):
        turn = {k: json.dumps(v) for k, v in TURN.items()}
        self.assertEqual(ruler.understanding(turn, SHEET)["queries"], 3)


class CompletenessTest(unittest.TestCase):
    def test_verdicts_and_totals(self):
        turn = {"content": "Risposta [1].\n\n---\n### Fonti e Riferimenti Verificati\n\n[1] x"}
        sheet = {"contents": ["a", "b", "c", "d"]}
        seen = []

        def ask(item):
            seen.append(item)
            return {"a": "COVERED", "b": "Verdict: UNCITED", "c": "DECLARED_MISSING", "d": "no idea"}[item["content"]]

        items = ruler.completeness(turn, sheet, ask)
        self.assertEqual([i["verdict"] for i in items], ["COVERED", "UNCITED", "DECLARED_MISSING", "ERROR"])
        self.assertNotIn("Fonti e Riferimenti", seen[0]["body"])  # the judge sees the body only
        self.assertIn("TOPIC: a", ruler.completeness_message(seen[0]))
        t = ruler.completeness_totals({"x": items})
        self.assertEqual((t["COVERED"], t["expected"], t["pct"]), (1, 4, 25.0))
        self.assertEqual(ruler.completeness_line(t), "completeness: 25.0% (COVERED 1/4, partial 0, uncited 1, "
                                                     "declared missing 1, absent 0, judge errors 1)")

    def test_empty_answer_and_no_contents(self):
        self.assertEqual(ruler.completeness({"content": ""}, {"contents": []}, None), [])
        items = ruler.completeness({"content": ""}, {"contents": ["a"]}, None)
        self.assertEqual(items[0]["verdict"], "ABSENT")


class RoundTest(unittest.TestCase):
    def test_measure_round_from_db(self):
        with tempfile.TemporaryDirectory() as tmp:
            db = os.path.join(tmp, "v.db")
            conn = sqlite3.connect(db)
            conn.execute("create table messages (session_id, role, content, sources_json, facts_json, "
                         "subqueries_json, meta_json, created_at)")
            conn.execute("insert into messages values ('s1', 'assistant', 'answer', '[]', ?, ?, '{}', 1)",
                         (json.dumps(TURN["facts"]), json.dumps(TURN["subqueries"])))
            conn.commit()
            with open(os.path.join(tmp, "runs_r_1.json"), "w") as f:
                json.dump({"niri": "s1", "gone": "s2", "nosheet": "s3"}, f)
            rec = ruler.measure_round("r_1", tmp, conn, {"niri": SHEET, "gone": SHEET},
                                      ask=lambda item: "ABSENT", passes=2)
            self.assertEqual(list(rec["understanding"]), ["niri"])
            self.assertEqual(len(rec["completeness_passes"]), 2)
            self.assertEqual(rec["completeness"]["niri"][0]["verdict"], "ABSENT")
            self.assertEqual(rec["problems"], ["session of gone not in the database", "no sheet for nosheet"])


class ExpectedFileTest(unittest.TestCase):
    def test_every_benchmark_case_has_a_sheet(self):
        here = os.path.dirname(os.path.abspath(__file__))
        with open(os.path.join(here, "benchmark.json")) as f:
            ids = {c["id"] for c in json.load(f)["cases"]}
        sheets = ruler.load_expected()
        self.assertEqual(ids, set(sheets))
        for case, sheet in sheets.items():
            n = len(sheet["contents"])
            self.assertTrue(n == 0 if case.startswith("unanswerable") else 3 <= n <= 8, case)


if __name__ == "__main__":
    unittest.main()
