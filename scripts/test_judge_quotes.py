#!/usr/bin/env python3
"""Tests of judge_quotes (Piano dopo audit, fase 6.5): the paragraph shown with a passage, the
sentences sent to the judge, the request and the summary. Stdlib only, no network, no judge.

Run: python3 -m unittest scripts/test_judge_quotes.py
"""
import contextlib
import hashlib
import io
import json
import os
import sqlite3
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import judge_pages as jp  # noqa: E402
import judge_quotes as jq  # noqa: E402

PAGE = ("Titolo della scheda del reattore\n\n"
        "Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate. Le pareti sono spesse "
        "venticinque centimetri.\n\n"
        "Gli operatori controllano il livello del refrigerante ogni mattina.")

OTHER_URL = "https://b.test/porto"
PAGE_URL = "https://a.test/reattore"
MISSING_URL = "https://c.test/assente"


def write_page(cache_dir, url, content):
    h = hashlib.sha256(jp.page_key(url).encode()).hexdigest()
    os.makedirs(os.path.join(cache_dir, "page", h[:2]), exist_ok=True)
    with open(os.path.join(cache_dir, "page", h[:2], h + ".json"), "w") as f:
        json.dump({"key": jp.page_key(url), "kind": "page", "document": {"url": url, "content": content}}, f)


QUOTE_ITEMS = [
    {"sentence": "Il recipiente pesa 310 tonnellate ed è di acciaio.", "state": "found", "numbers_out": [],
     "refs": [{"url": PAGE_URL, "quote": "pesa 310 tonnellate", "found": True,
               "evidence": "Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate."},
              {"url": MISSING_URL, "quote": "un altro passo", "found": True, "evidence": "Un passo di una pagina che la cache non ha."}]},
    {"sentence": "Le pareti sono spesse 30 centimetri.", "state": "found", "numbers_out": ["30"],
     "refs": [{"url": OTHER_URL, "quote": "un passo che non c'è", "found": False},
              {"url": PAGE_URL, "quote": "pareti spesse", "found": True, "evidence": "Le pareti sono spesse venticinque centimetri."}]},
    {"sentence": "Una frase con un passo che nessuna pagina ha.", "state": "not_found",
     "refs": [{"url": PAGE_URL, "quote": "niente", "found": False}]},
    {"sentence": "Una deduzione del modello.", "state": "none"},
]


class Paragraph(unittest.TestCase):
    def test_the_paragraph_that_holds_the_passage(self):
        got = jq.paragraph(PAGE, "pesa 310 tonnellate. Le pareti")
        self.assertEqual(got, "Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate. "
                              "Le pareti sono spesse venticinque centimetri.")

    def test_a_passage_across_a_line_break_is_placed_by_characters(self):
        page = "Prima riga del testo,\nseconda riga con il passo cercato qui.\n\nUn altro paragrafo."
        got = jq.paragraph(page, "Prima riga del testo, seconda riga con il passo")
        self.assertIn("Prima riga del testo, seconda riga con il passo", got)
        self.assertLessEqual(len(got), jq.PARA_CHARS)

    def test_a_passage_not_on_the_page(self):
        self.assertEqual(jq.paragraph(PAGE, "una frase che la pagina non contiene affatto"), "")
        self.assertEqual(jq.paragraph(None, "pesa 310 tonnellate"), "")
        self.assertEqual(jq.paragraph(PAGE, ""), "")

    def test_a_passage_across_two_paragraphs_is_shown_whole(self):
        """The evidence Go saves can run over a paragraph break: every paragraph it touches is shown
        (Piano fase 6 chiusura, A8). Before, only the first one, when it was longer than the evidence:
        in the rounds quotes_1 and quotesconv_1, 41 of 643 sentences judged had no passage shown whole."""
        page = ("Titolo.\n\n" + "Una lunga premessa della scheda. " * 10 +
                "Le pareti sono spesse venticinque centimetri.\n\n"
                "Gli operatori controllano il livello del refrigerante ogni mattina.\n\nFine.")
        evidence = "Le pareti sono spesse venticinque centimetri. Gli operatori controllano il livello del refrigerante"
        got = jq.paragraph(page, evidence)
        self.assertIn(evidence, got)
        self.assertTrue(got.startswith("Una lunga premessa"))
        self.assertTrue(got.endswith("ogni mattina."))

    def test_two_long_paragraphs_are_cut_around_the_whole_passage(self):
        key = "un passo che comincia dentro il primo paragrafo e ne raggiunge la fine.\n\nE l'inizio del secondo, che il passo tocca"
        page = "Introduzione.\n\n" + "prima " * 400 + key + " dopo" * 400 + "\n\nFine."
        got = jq.paragraph(page, key)
        self.assertEqual(len(got), jq.PARA_CHARS)
        self.assertIn("ne raggiunge la fine. E l'inizio del secondo, che il passo tocca", got)

    def test_a_passage_in_two_parts_close_together(self):
        """Go joins the parts of a quote with an ellipsis with " … " (evidence.go evidenceText)."""
        evidence = "Il recipiente del reattore è di acciaio forgiato … Le pareti sono spesse venticinque centimetri."
        got = jq.paragraph(PAGE, evidence)
        self.assertEqual(got, "Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate. "
                              "Le pareti sono spesse venticinque centimetri.")

    def test_a_passage_in_two_parts_far_apart(self):
        page = ("Introduzione.\n\nLa prima parte del passo sta qui.\n\n" + "riempitivo " * 300 +
                "\n\nLa seconda parte del passo sta molto più in basso.\n\nFine.")
        got = jq.paragraph(page, "La prima parte del passo sta qui. … La seconda parte del passo sta molto più in basso.")
        self.assertIn("La prima parte del passo sta qui.", got)
        self.assertIn("La seconda parte del passo sta molto più in basso.", got)
        self.assertLessEqual(len(got), jq.PARA_CHARS + len(" … "))

    def test_a_long_paragraph_is_cut_around_the_passage(self):
        key = "il passo da ritrovare nel mezzo di un paragrafo molto lungo"
        page = "Introduzione.\n\n" + "parola " * 400 + key + " altra" * 400 + "\n\nFine."
        got = jq.paragraph(page, key)
        self.assertEqual(len(got), jq.PARA_CHARS)
        self.assertIn(key, got)


class UnitsOf(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.cache = self.tmp.name
        write_page(self.cache, PAGE_URL, PAGE)

    def tearDown(self):
        self.tmp.cleanup()

    def turn(self, items):
        return {"sid": "sid-1", "meta": {"loop": {"quote_items": items}}}

    def test_only_found_sentences_with_their_found_passages(self):
        units = jq.units_of(self.turn(QUOTE_ITEMS), self.cache)
        self.assertEqual([u["sentence"] for u in units], [QUOTE_ITEMS[0]["sentence"], QUOTE_ITEMS[1]["sentence"]])
        first, second = units
        # two passages; the page in the cache gives the paragraph, the one missing gives the evidence alone
        self.assertEqual(len(first["passages"]), 2)
        self.assertIn("Le pareti sono spesse venticinque centimetri.", first["passages"][0])
        self.assertEqual(first["passages"][1], "Un passo di una pagina che la cache non ha.")
        self.assertEqual(first["chars"], sum(len(p) for p in first["passages"]))
        # the passage not found is not shown
        self.assertEqual(len(second["passages"]), 1)
        self.assertEqual(second["numbers_out"], ["30"])
        self.assertEqual(first["sid"], "sid-1")

    def test_without_a_cache_the_evidence(self):
        units = jq.units_of(self.turn(QUOTE_ITEMS[:1]), None)
        self.assertEqual(units[0]["passages"], [QUOTE_ITEMS[0]["refs"][0]["evidence"], QUOTE_ITEMS[0]["refs"][1]["evidence"]])

    def test_no_items(self):
        self.assertEqual(jq.units_of({"sid": "s", "meta": {"loop": {}}}, self.cache), [])
        self.assertEqual(jq.units_of({"sid": "s", "meta": {}}, self.cache), [])


class Message(unittest.TestCase):
    def test_numbered_sentences_and_their_passages(self):
        units = [{"sentence": "Prima frase.", "passages": ["Passo uno.", "Passo due."]},
                 {"sentence": "Seconda frase.", "passages": ["Passo tre."]}]
        self.assertEqual(jq.message(units),
                         "SENTENCES:\n"
                         "1. Prima frase.\n   PASSAGE: Passo uno.\n   PASSAGE: Passo due.\n"
                         "2. Seconda frase.\n   PASSAGE: Passo tre.")


class Summarize(unittest.TestCase):
    def test_split_by_numbers_out(self):
        units = [{"verdict": "SUPPORTED", "numbers_out": []},
                 {"verdict": "SUPPORTED", "numbers_out": []},
                 {"verdict": "PARTIAL", "numbers_out": []},
                 {"verdict": "ABSENT", "numbers_out": ["30"]},
                 {"verdict": "SUPPORTED", "numbers_out": ["2019"]},
                 {"numbers_out": []}]  # no verdict: the judge's line was missing
        s = jq.summarize(units)
        self.assertEqual((s["all"]["judged"], s["all"]["SUPPORTED"], s["all"]["ERROR"]), (5, 3, 1))
        self.assertEqual(s["all"]["pct_supported"], 60.0)
        inside, outside = s["numbers all inside the passage"], s["some number not in the passage"]
        self.assertEqual((inside["judged"], inside["SUPPORTED"], inside["PARTIAL"], inside["ERROR"]), (3, 2, 1, 1))
        self.assertEqual((outside["judged"], outside["SUPPORTED"], outside["ABSENT"]), (2, 1, 1))
        self.assertEqual(outside["pct_supported"], 50.0)

    def test_nothing_judged(self):
        self.assertIsNone(jq.summarize([])["all"]["pct_supported"])


class DryRun(unittest.TestCase):
    def test_counts_and_calls_no_one(self):
        with tempfile.TemporaryDirectory() as d:
            cache = os.path.join(d, "cache")
            write_page(cache, PAGE_URL, PAGE)
            db = os.path.join(d, "v.db")
            conn = sqlite3.connect(db)
            conn.execute("CREATE TABLE messages (session_id TEXT, role TEXT, query TEXT, content TEXT, mode TEXT, "
                         "sources_json TEXT, meta_json TEXT, created_at TEXT)")
            meta = {"loop": {"calls": 3, "quotes": "done", "quote_items": QUOTE_ITEMS}}
            conn.execute("INSERT INTO messages VALUES ('sid-1','assistant','Domanda?','Risposta.','deep','[]',?,'2026-11-16')",
                         (json.dumps(meta),))
            conn.commit()
            conn.close()
            runs = os.path.join(d, "runs_r.json")
            with open(runs, "w") as f:
                json.dump({"case-a": "sid-1"}, f)
            out = os.path.join(d, "out")
            argv = ["judge_quotes.py", "r", "--db", db, "--cache", cache, "--runs", runs, "--dry-run"]
            never = mock.Mock(side_effect=AssertionError("the judge was called"))
            printed = io.StringIO()
            with mock.patch.object(sys, "argv", argv), mock.patch.object(jq, "OUT", out), \
                    mock.patch.object(jq.judge, "judge_config", never), mock.patch.object(jq.judge, "make_ask", never), \
                    contextlib.redirect_stdout(printed):
                jq.main()
            never.assert_not_called()
            self.assertIn("r: 1 answers, 2 found sentences, 1 judge calls", printed.getvalue())
            self.assertFalse(os.path.exists(out))


if __name__ == "__main__":
    unittest.main()
