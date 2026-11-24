#!/usr/bin/env python3
"""Tests of judge_links: which sentence a link belongs to and what happened to its page."""
import json
import os
import sqlite3
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import judge_links as jl  # noqa: E402


class ClaimUnits(unittest.TestCase):
    def test_links_stay_with_their_sentence(self):
        u = jl.claim_units("Prima frase senza link. Seconda frase lunga con [fonte](https://a.test/x).")
        self.assertEqual(len(u), 1)
        self.assertIn("Seconda frase", u[0]["text"])
        self.assertEqual(u[0]["links"], ["https://a.test/x"])
        self.assertIn("Prima frase", u[0]["context"])

    def test_a_piece_of_links_cites_the_sentence_before(self):
        u = jl.claim_units("Una frase abbastanza lunga da essere giudicata. ([docs](https://a.test/d), [blog](https://b.test/p)).")
        self.assertEqual(len(u), 1)
        self.assertIn("Una frase", u[0]["text"])
        self.assertEqual(u[0]["links"], ["https://a.test/d", "https://b.test/p"])

    def test_a_piece_made_of_link_words_cites_the_sentence_before(self):
        u = jl.claim_units("Il Se bipolare nasce dal lavoro degli anni Settanta di Kohut. ([Self psychology – Wikipedia EN](https://w.test/s)).")
        self.assertEqual(len(u), 1)
        self.assertIn("Il Se bipolare", u[0]["text"])
        self.assertEqual(u[0]["links"], ["https://w.test/s"])

    def test_a_sentence_cut_at_an_abbreviation_goes_on(self):
        u = jl.claim_units("Lo prevede la legge n. 633 ([testo](https://n.test/633)) con sanzioni pesanti per chi scarica.")
        self.assertEqual(len(u), 1)
        self.assertIn("633", u[0]["text"])
        self.assertIn("sanzioni", u[0]["text"])

    def test_a_link_alone_on_its_line_is_a_unit(self):
        u = jl.claim_units("Il progetto fa molte cose utili per la casa.\n\n- [github.com/x/y](https://github.com/x/y)")
        self.assertEqual(len(u), 1)
        self.assertEqual(u[0]["text"], "github.com/x/y")
        self.assertIn("Il progetto", u[0]["context"])

    def test_addresses_in_code_are_not_links(self):
        self.assertEqual(jl.claim_units("Apri `http://localhost:3000` nel browser e poi continua pure."), [])

    def test_table_row_is_whole(self):
        u = jl.claim_units("| Licenza | MIT | [fonte](https://a.test/l) |")
        self.assertEqual(len(u), 1)
        self.assertIn("MIT", u[0]["text"])

    def test_headings_are_skipped(self):
        self.assertEqual(jl.claim_units("## Titolo con [link](https://a.test/t)"), [])

    def test_notes_are_not_the_answer(self):
        body = jl.answer_body("Testo della risposta con [a](https://a.test/a).\n\n<!-- vedetta:notes -->\n\n### Pagine lette\n\n1. [b](https://b.test/b)")
        self.assertNotIn("b.test", body)
        self.assertIn("a.test", body)


class PageClasses(unittest.TestCase):
    TURN = {
        "content": "Frase uno lunga abbastanza con [a](https://a.test/read). Frase due lunga abbastanza con [b](https://b.test/prev), e [c](https://c.test/vet).\n\nFrase tre lunga abbastanza con [d](https://d.test/out) e ancora [a](https://www.a.test/read/).",
        "sid": "s",
        "sources": [{"url": "https://a.test/read"}, {"url": "https://b.test/prev"}, {"url": "https://c.test/vet"}],
        "meta": {"loop": {
            "calls_detail": [{"tool": "read", "url": "https://a.test/read", "result": "ok"},
                             {"tool": "read", "url": "https://b.test/prev", "result": "unreadable"}],
            "review_items": [{"url": "https://c.test/vet"}, {"url": "https://b.test/prev", "unread": True}],
        }},
    }

    def test_classes(self):
        got = {p["url"]: p["cls"] for p in jl.pairs_of(self.TURN)}
        self.assertEqual(got, {"https://a.test/read": "read", "https://b.test/prev": "preview",
                               "https://c.test/vet": "vedetta", "https://d.test/out": "other"})

    def test_a_page_vedetta_checked_is_not_a_preview(self):
        turn = dict(self.TURN, meta={"loop": {"link_states": {"https://b.test/prev": "checked", "https://d.test/out": "unread"}}})
        got = jl.page_classes(turn)
        self.assertEqual(got["b.test/prev"], "vedetta")
        self.assertNotEqual(got.get("d.test/out"), "vedetta")

    def test_same_page_twice_is_one_pair_with_two_sentences(self):
        pair = next(p for p in jl.pairs_of(self.TURN) if p["cls"] == "read")
        self.assertEqual(len(pair["units"]), 2)


class Verdicts(unittest.TestCase):
    def test_parse(self):
        raw = "1: SUPPORTED\n**2**: partial\n3. ABSENT\nnoise\n5: POINTER"
        self.assertEqual(jl.parse_verdicts(raw, 5), ["SUPPORTED", "PARTIAL", "ABSENT", "ERROR", "POINTER"])

    def test_a_page_that_does_not_open_is_not_a_verdict(self):
        pairs = [{"cls": "preview", "status": "blocked: anubis", "units": [{"text": "x", "others": 0}]},
                 {"cls": "preview", "status": "ok", "units": [{"text": "y", "verdict": "SUPPORTED", "others": 0}, {"text": "z", "verdict": "ABSENT", "others": 1}, {"text": "w", "verdict": "POINTER", "others": 0}]}]
        row = jl.summarize(pairs)["preview"]
        self.assertEqual((row["UNREADABLE"], row["judged"], row["pct_supported"], row["POINTER"]), (1, 2, 50.0, 1))
        one = jl.summarize(pairs, single_only=True)["preview"]
        self.assertEqual((one["judged"], one["pct_supported"]), (1, 100.0))


class AddressLinks(unittest.TestCase):
    def turn(self, content):
        return {"content": content, "sid": "s", "sources": [], "meta": {"loop": {}}}

    def test_a_link_showing_its_address_is_a_pointer_not_sent(self):
        p = jl.pairs_of(self.turn("Ideale per il tuo piccolo server di casa. [github.com/x/y](https://github.com/x/y)"))[0]
        self.assertEqual((p["units"][0]["verdict"], p["units"][0]["by"]), ("POINTER", "address"))
        self.assertEqual(jl.to_judge(p), [])

    def test_a_site_named_as_a_source_is_a_citation(self):
        p = jl.pairs_of(self.turn("Le villette sono escluse dal 2024 salvo pochi casi ([bonusepensioneoggi.it](https://b.test/a))."))[0]
        self.assertNotIn("verdict", p["units"][0])

    def test_an_address_does_not_count_as_a_second_source(self):
        pairs = jl.pairs_of(self.turn("Costa cinque euro al mese secondo il listino [listino](https://l.test/p) e il sito [l.test/home](https://l.test/home)."))
        cited = next(p for p in pairs if p["url"].endswith("/p"))
        self.assertEqual(cited["units"][0]["others"], 0)


class BySentence(unittest.TestCase):
    def test_best_verdict_and_whether_a_page_was_read(self):
        pairs = [
            {"sid": "s", "cls": "read", "status": "cache", "units": [{"text": "uno", "verdict": "ABSENT"}]},
            {"sid": "s", "cls": "preview", "status": "ok", "units": [{"text": "uno", "verdict": "SUPPORTED"}, {"text": "due", "verdict": "PARTIAL"}]},
            {"sid": "s", "cls": "preview", "status": "error: 403", "units": [{"text": "due"}, {"text": "tre"}]},
        ]
        t = jl.by_sentence(pairs)
        self.assertEqual((t["some page read"]["SUPPORTED"], t["some page read"]["judged"]), (1, 1))
        self.assertEqual((t["no page read"]["PARTIAL"], t["no page read"]["UNREADABLE"], t["no page read"]["judged"]), (1, 1, 1))


class BenchRounds(unittest.TestCase):
    """Piano dopo audit 5.1: the bench gives judge_links its own runs file and prints a saved judging."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.db = os.path.join(self.tmp.name, "vedetta.db")
        conn = sqlite3.connect(self.db)
        conn.execute("create table messages (session_id text, role text, mode text, query text, content text, "
                     "sources_json text, meta_json text, created_at text)")
        meta = json.dumps({"loop": {"calls": 3}})
        for i, sid in enumerate(("sid-a", "sid-b", "sid-other")):
            conn.execute("insert into messages values (?, 'assistant', 'deep', 'q', 'testo', '[]', ?, ?)", (sid, meta, f"2026-11-13 10:0{i}"))
        conn.commit()
        conn.close()

    def tearDown(self):
        self.tmp.cleanup()

    def runs_file(self, data):
        path = os.path.join(self.tmp.name, "runs.json")
        with open(path, "w") as f:
            json.dump(data, f)
        return path

    def test_the_runs_of_a_bench_round_name_its_sessions(self):
        turns = jl.load_turns(self.db, self.runs_file({"case-a": "sid-a", "case-b": "sid-b"}))
        self.assertEqual([t["sid"] for t in turns], ["sid-a", "sid-b"])

    def test_a_list_of_runs_works_as_before(self):
        turns = jl.load_turns(self.db, self.runs_file([{"sid": "sid-a", "v": "con"}, {"sid": "sid-b", "v": "senza"}]), "v=senza")
        self.assertEqual([t["sid"] for t in turns], ["sid-b"])

    def test_a_saved_judging_prints_the_same_tables(self):
        pairs = [{"sid": "s", "cls": "read", "status": "cache", "units": [{"text": "uno", "verdict": "SUPPORTED", "others": 0}]},
                 {"sid": "s", "cls": "other", "status": "ok", "units": [{"text": "due", "verdict": "ABSENT", "others": 0}]}]
        saved = {"summary": jl.summarize(pairs), "summary_one_link": jl.summarize(pairs, single_only=True),
                 "summary_sentences": jl.by_sentence(pairs)}
        text = jl.format_summary(json.loads(json.dumps(saved)))
        self.assertIn("all sentences", text)
        self.assertIn("sentences with one link", text)
        self.assertIn("each sentence once", text)
        read = next(line for line in text.splitlines() if line.strip().startswith("read"))
        self.assertIn("SUPPORTED    1 (100.0%)", read)
        self.assertIn("no page read", text)


class WholePage(unittest.TestCase):
    """Piano dopo audit 5.2: the judge gets the whole page, up to the limit of judge_pages."""

    UNITS = [{"text": "La frase da giudicare.", "context": ""}]

    def test_the_end_of_a_long_page_reaches_the_judge(self):
        page = "a" * 50000 + " IL PASSO IN FONDO"
        self.assertIn("IL PASSO IN FONDO", jl.message("https://p.test/x", page, self.UNITS))

    def test_the_limit_is_the_one_of_the_page_judge(self):
        self.assertEqual(jl.PAGE_CHARS, jl.jp.TOTAL_CHARS)
        page = "a" * jl.PAGE_CHARS + " OLTRE IL TETTO"
        self.assertNotIn("OLTRE IL TETTO", jl.message("https://p.test/x", page, self.UNITS))

    def test_a_judging_says_how_much_of_a_page_the_judge_saw(self):
        pairs = [{"sid": "s", "cls": "read", "status": "cache", "units": [{"text": "uno", "verdict": "SUPPORTED", "others": 0}]}]
        saved = {"summary": jl.summarize(pairs), "summary_one_link": jl.summarize(pairs, single_only=True),
                 "summary_sentences": jl.by_sentence(pairs)}
        old = jl.format_summary(saved)  # judged before 5.2: the file does not say, it was 40,000
        self.assertIn("first 40000 characters", old)
        self.assertIn("not comparable", old)
        new = jl.format_summary({**saved, "page_chars": jl.PAGE_CHARS})
        self.assertIn(f"first {jl.PAGE_CHARS} characters", new)
        self.assertNotIn("not comparable", new)

    def test_pages_cut_are_counted(self):
        pairs = [{"status": "cache", "text": "a" * (jl.PAGE_CHARS + 1), "units": [{"text": "x"}]},
                 {"status": "ok", "text": "breve", "units": [{"text": "y"}]},
                 {"status": "error: 403", "text": "", "units": [{"text": "z"}]}]
        self.assertEqual(jl.pages_cut(pairs), 1)


if __name__ == "__main__":
    unittest.main()
