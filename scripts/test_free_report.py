#!/usr/bin/env python3
"""Unit tests for scripts/free_report.py (Piano dopo audit 2.2). Stdlib only, no network.

Run: python3 -m unittest scripts/test_free_report.py
"""
import json
import os
import sqlite3
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import free_report as fr  # noqa: E402

ANSWER = (
    "La prima frase spiega il tema con abbastanza parole da contare. "
    "La seconda frase porta una fonte accanto a sé [pagina](https://official.test/page). "
    "La terza frase aggiunge un dettaglio senza nessun link vicino.\n\n"
    "La quarta frase, in un altro paragrafo, resta anche lei senza fonte.\n\n"
    "<!-- vedetta:notes -->\n\n---\n*Ricerca guidata dal modello: 4 ricerche, 1 pagina letta, 1 non leggibile.*"
)

CALLS = [
    {"tool": "search", "query": "tema principale spiegato", "lang": "it", "result": "ok"},
    {"tool": "search", "query": '"frase esatta del tema" altro', "result": "empty"},
    {"tool": "search", "query": '"prima frase" "seconda frase"', "result": "empty"},
    {"tool": "search", "query": "site:official.test tema", "lang": "it", "result": "ok"},  # the last search showing the page
    {"tool": "read", "url": "https://official.test/page", "result": "ok"},
    {"tool": "read", "url": "https://other.test/p", "result": "unreadable"},
    {"tool": "read", "url": "https://official.test/page", "result": "repeated"},
]


# The quote turn's sentences as Go saves them (linkClaim.Text): the words of a link stay, its
# address does not. The links are in the answer's text.
QUOTE_ANSWER = (
    "La seconda frase porta una fonte accanto a sé [pagina](https://official.test/page). "
    "Una frase con un passo che nessuna pagina ha, scritto dal **modello**. "
    "Una frase che rimanda a una pagina aperta da Vedetta [guida](https://opened.test/g) e basta. "
    "Una deduzione del modello, senza nessun link vicino a lei.\n\n"
    "Una frase oltre il tetto di quelle chieste al modello in tutto.\n\n"
    "<!-- vedetta:notes -->\n\n---\n*Frasi con una citazione ritrovata nelle pagine lette: 1 di 4.*"
)

QUOTE_ITEMS = [
    {"sentence": "La seconda frase porta una fonte accanto a sé pagina.", "state": "found"},
    {"sentence": "Una frase con un passo che nessuna pagina ha, scritto dal modello.", "state": "not_found"},
    {"sentence": "Una frase con un passo di due parole, che nessuno cerca nelle pagine.", "state": "short"},
    {"sentence": "Una frase che rimanda a una pagina aperta da Vedetta guida e basta.", "state": "none"},
    {"sentence": "Una deduzione del modello, senza nessun link vicino a lei.", "state": "none"},
    {"sentence": "Una frase che il testo della risposta non ha più, scritta altrove.", "state": "none"},
    {"sentence": "Una frase oltre il tetto di quelle chieste al modello in tutto.", "state": "unasked"},
]


class FreeReportTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        d = self.tmp.name
        self.out = os.path.join(d, "out")
        os.makedirs(self.out)
        with open(os.path.join(self.out, "runs_r1.json"), "w") as f:
            json.dump({"case-a": "sid-1"}, f)
        self.expected = os.path.join(d, "expected.json")
        with open(self.expected, "w") as f:
            json.dump({"cases": {"case-a": {"entities": [
                {"name": "Ufficio", "home": ["official.test"]},
                {"name": "Senza casa"},
            ]}}}, f)
        self.db = os.path.join(d, "v.db")
        conn = sqlite3.connect(self.db)
        conn.execute("CREATE TABLE messages (id TEXT, session_id TEXT, role TEXT, query TEXT, content TEXT, mode TEXT, "
                     "sources_json TEXT, meta_json TEXT, created_at TEXT)")
        meta = {"partial": False, "loop": {"closed_by": "answer", "calls_detail": CALLS}}
        sources = [{"url": "https://official.test/page", "title": "Pagina"}, {"url": "https://news.test/a", "title": "A"}]
        conn.execute("INSERT INTO messages VALUES ('m1','sid-1','assistant','Che cosa dice il tema?',?, 'deep', ?, ?, '2026-11-11')",
                     (ANSWER, json.dumps(sources), json.dumps(meta)))
        conn.commit()
        conn.close()
        self.cache_root = os.path.join(d, "data")
        cache = os.path.join(self.cache_root, "cache-live-r1")
        for kind, name, entry in (
            ("search", "s1.json", {"key": 'v1|search|searxng|"tema principale spiegato"|lang=it|cat=|max=10|bio=false|auth=',
                                   "kind": "search", "results": [{"url": "https://official.test/page"}, {"url": "https://news.test/a"}]}),
            ("search", "s2.json", {"key": 'v1|search|searxng|"site:official.test tema"|lang=it|cat=|max=10|bio=false|auth=',
                                   "kind": "search", "results": [{"url": "https://official.test/page"}]}),
            ("page", "p1.json", {"key": "v1|page|https://official.test/page", "kind": "page",
                                 "document": {"title": "Pagina non trovata", "url": "https://official.test/page",
                                              "content": "Errore 404: la pagina che cerchi non esiste."}}),
        ):
            os.makedirs(os.path.join(cache, kind, "ab"), exist_ok=True)
            with open(os.path.join(cache, kind, "ab", name), "w") as f:
                json.dump(entry, f)

    def tearDown(self):
        self.tmp.cleanup()

    def round(self):
        return fr.compute_round("r1", self.out, self.db, self.cache_root, fr.load_expected(self.expected))

    def test_sentences_with_a_source(self):
        r = self.round()
        self.assertEqual((r["sentences"]["claims"], r["sentences"]["linked"]), (4, 1))
        self.assertAlmostEqual(r["sentences"]["pct"], 25.0)
        self.assertEqual((r["sentences"]["paragraphs"], r["sentences"]["paragraphs_linked"]), (2, 1))

    def test_searches_by_type(self):
        s = self.round()["searches"]
        self.assertEqual(s["plain"], {"n": 1, "empty": 0})
        self.assertEqual(s["one_quote"], {"n": 1, "empty": 1})
        self.assertEqual(s["more_quotes"], {"n": 1, "empty": 1})
        self.assertEqual(s["site"], {"n": 1, "empty": 0})

    def test_official_page(self):
        o = self.round()["official"]
        self.assertEqual((o["pairs"], o["in_results"], o["read"]), (1, 1, 1))

    def test_reads_and_error_pages(self):
        r = self.round()
        self.assertEqual((r["reads"]["ok"], r["reads"]["unreadable"], r["reads"]["repeated"]), (1, 1, 1))
        self.assertEqual(r["reads"]["outside_results"], 1)  # other.test/p came from no search
        self.assertEqual(r["error_pages"], 1)

    def test_where_each_sentence_comes_from(self):
        """Piano dopo audit 6.3: the quote turn's counts, and the `none` ones in three kinds."""
        conn = sqlite3.connect(self.db)
        meta = {"partial": False, "loop": {"closed_by": "answer", "calls_detail": CALLS, "quotes": "done", "quote_items": QUOTE_ITEMS,
                                           "quote_seconds": 40, "link_states": {"https://opened.test/g": "checked"}}}
        conn.execute("UPDATE messages SET meta_json = ?, content = ?", (json.dumps(meta), QUOTE_ANSWER))
        conn.commit()
        conn.close()
        q = self.round()["quotes"]
        self.assertEqual((q["asked"], q["found"], q["not_found"], q["short"], q["none"], q["unasked"]), (6, 1, 1, 1, 3, 1))
        self.assertEqual(q["answers"], {"done": 1})
        self.assertEqual(q["seconds_median"], 40)
        # the link is taken from the answer's text; the sentence the text no longer has goes with the others
        self.assertEqual(q["none_kinds"], {"link_to_opened_page": 1, "earlier_pages": 0, "other": 2})
        self.assertEqual(q["none_unmatched"], 1)
        table = fr.format_table([self.round()])
        self.assertIn("sentences with a quote found", table)
        self.assertIn("1 / 0 / 2 (not found in the answer: 1)", table)

    def test_none_in_a_follow_up(self):
        """A `none` sentence of a follow-up with pages of the earlier turns, and no link to an opened page."""
        conn = sqlite3.connect(self.db)
        meta = {"partial": False, "loop": {"closed_by": "answer", "calls_detail": CALLS, "quotes": "done", "quote_items": QUOTE_ITEMS,
                                           "session_pages": 3, "link_states": {"https://official.test/page": "checked"}}}
        conn.execute("UPDATE messages SET meta_json = ?, content = ?", (json.dumps(meta), QUOTE_ANSWER))
        conn.commit()
        conn.close()
        q = self.round()["quotes"]
        self.assertEqual(q["none_kinds"], {"link_to_opened_page": 0, "earlier_pages": 2, "other": 1})
        self.assertEqual(q["none_unmatched"], 1)

    def test_no_quotes_without_the_switch(self):
        r = self.round()
        self.assertEqual(r["quotes"]["asked"], 0)
        self.assertIn("n/a", [l for l in fr.format_table([r]).split("\n") if l.startswith("sentences with a quote found")][0])

    def test_closing(self):
        c = self.round()["closing"]
        self.assertEqual(c["closed_by"], {"answer": 1})
        self.assertEqual((c["answers"], c["partial"], c["reviewed"]), (1, 0, 0))

    def test_table_side_by_side_and_saved(self):
        r = self.round()
        text = fr.format_table([r, dict(r, name="r2")])
        self.assertIn("r1", text.split("\n")[0])
        self.assertIn("r2", text.split("\n")[0])
        self.assertIn("25.0%", text)
        fr.save_round(r, self.out)
        with open(os.path.join(self.out, "free_r1.json")) as f:
            self.assertEqual(json.load(f)["sentences"]["linked"], 1)

    def test_pages_in_the_language_of_their_search(self):
        """Plan 3.3: a page read, found by a search in Italian, is in Italian."""
        self.assertEqual(self.round()["lang_match"], {"pages": 1, "same": 1, "query_lang_unknown": 0})

    def test_detect_lang(self):
        self.assertEqual(fr.detect_lang("la pagina che cerchi non esiste ed è stata tolta"), "it")
        self.assertEqual(fr.detect_lang("the page you are looking for does not exist"), "en")
        self.assertEqual(fr.detect_lang("komodo release 1.19"), "")

    def test_answer_saved_without_sources(self):
        """A message can be saved with sources_json "null" (seen in r33 rounds, 11/11)."""
        conn = sqlite3.connect(self.db)
        conn.execute("UPDATE messages SET sources_json = 'null'")
        conn.commit()
        conn.close()
        r = self.round()
        self.assertEqual(r["answers"], 1)
        self.assertEqual(r["official"]["in_results"], 0)

    def test_round_without_cache_says_so(self):
        r = fr.compute_round("r1", self.out, self.db, os.path.join(self.tmp.name, "nowhere"), fr.load_expected(self.expected))
        self.assertIsNone(r["error_pages"])
        self.assertIsNone(r["reads"]["outside_results"])
        self.assertIsNone(r["lang_match"])
        self.assertEqual(r["sentences"]["linked"], 1)


NOTES = "\n\n<!-- vedetta:notes -->\n\n---\n*Ricerca guidata dal modello: 1 ricerca, 1 pagina letta.*"


class ConversationsTest(unittest.TestCase):
    """Piano dopo audit 5.3: the rows of a conversation, by turn (docs/PROPOSTA_5_3_CONVERSAZIONI.md §4)."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        d = self.tmp.name
        self.out = os.path.join(d, "out")
        os.makedirs(self.out)
        with open(os.path.join(self.out, "runs_c1.json"), "w") as f:
            json.dump({"conv-a": "sid-a", "single": "sid-s"}, f)
        with open(os.path.join(self.out, "turns_c1.json"), "w") as f:
            json.dump({"conv-a": [{"q": "uno", "seconds": 100}, {"q": "due", "seconds": 400}, {"q": "tre", "seconds": 300}]}, f)
        self.expected = os.path.join(d, "expected.json")
        with open(self.expected, "w") as f:
            json.dump({"cases": {}}, f)
        self.db = os.path.join(d, "v.db")
        conn = sqlite3.connect(self.db)
        conn.execute("CREATE TABLE messages (id TEXT, session_id TEXT, role TEXT, query TEXT, content TEXT, mode TEXT, "
                     "sources_json TEXT, meta_json TEXT, created_at TEXT)")

        def read(u, result="ok"):
            return {"tool": "read", "url": u, "result": result}
        search = {"tool": "search", "query": "il lago e la sua storia", "lang": "it", "result": "ok"}
        turns = [
            # 1: reads the first page, nothing flagged
            ("Il lago si è ridotto molto negli anni e la sua storia è nota a tutti." + NOTES,
             {"loop": {"closed_by": "answer", "searches": 1, "calls_detail": [search, read("https://a.test/1")]}}),
            # 2: reads the first page again and a new one; two numbers flagged, one is on the page read at turn 1;
            #    the draft was sent back
            ("La parte a nord è risalita e il livello è di nuovo più alto di prima." + NOTES,
             {"loop": {"closed_by": "answer", "searches": 2, "reviewed": True,
                       "calls_detail": [search, search, read("https://a.test/1"), read("https://a.test/2"), read("https://a.test/3", "unreadable")],
                       "numbers_missing": [{"sentence": "La superficie era di 68000 km2 e ora è di 777 km2.", "missing": ["68000", "777"]}]}}),
            # 3: answers in English and writes one of Vedetta's notes itself
            ("The numbers of the surface come from the first page and they are from the past.\n\n"
             "*Link nella risposta: 2. A pagine lette dal modello: 2.*" + NOTES,
             {"loop": {"closed_by": "answer", "searches": 0, "calls_detail": [],
                       "draft": "*Ricerca guidata dal modello: 2 ricerche.*"}}),
        ]
        for i, (content, meta) in enumerate(turns):
            conn.execute("INSERT INTO messages VALUES (?, 'sid-a', 'assistant', 'q', ?, 'deep', '[]', ?, ?)",
                         (f"m{i}", content, json.dumps(meta), f"2026-11-13 10:0{i}"))
        conn.execute("INSERT INTO messages VALUES ('ms', 'sid-s', 'assistant', 'q', ?, 'deep', '[]', ?, '2026-11-13 11:00')",
                     ("Una risposta sola, che non è una conversazione, ma conta." + NOTES,
                      json.dumps({"loop": {"closed_by": "answer", "searches": 1, "calls_detail": [search]}})))
        conn.commit()
        conn.close()
        self.cache_root = os.path.join(d, "data")
        cache = os.path.join(self.cache_root, "cache-live-c1", "page", "ab")
        os.makedirs(cache)
        for name, url, text in (("p1.json", "https://a.test/1", "Nel 1960 la superficie era di 68000 km2."),
                                ("p2.json", "https://a.test/2", "La diga ha fatto risalire il livello.")):
            with open(os.path.join(cache, name), "w") as f:
                json.dump({"key": "v1|page|" + url, "kind": "page", "document": {"title": "T", "url": url, "content": text}}, f)
        self.asked = []

    def tearDown(self):
        self.tmp.cleanup()

    def still_missing(self, sentences, evidence):
        """In place of the Go check: a number is still missing when the text does not hold its digits."""
        self.asked.append((sentences, evidence))
        import re
        return [n for s in sentences for n in re.findall(r"\d{3,}", s) if n not in evidence]

    def round(self, **kw):
        kw.setdefault("languages", {"conv-a": "it"})
        kw.setdefault("still_missing", self.still_missing)
        return fr.compute_round("c1", self.out, self.db, self.cache_root, fr.load_expected(self.expected), **kw)

    def test_every_answer_of_a_session_counts(self):
        r = self.round()
        self.assertEqual(r["answers"], 4)
        self.assertEqual(r["reads"]["ok"], 3)
        self.assertEqual(r["closing"]["reviewed"], 1)

    def test_rows_by_turn(self):
        t = self.round()["turns"]
        self.assertEqual([x["turn"] for x in t], [1, 2, 3])
        self.assertEqual([x["answers"] for x in t], [1, 1, 1])  # the single answer is not a conversation
        self.assertEqual([x["seconds_median"] for x in t], [100, 400, 300])
        self.assertEqual([x["searches"] for x in t], [1, 2, 0])
        self.assertEqual([x["pages_read"] for x in t], [1, 2, 0])
        self.assertEqual([x["pages_read_again"] for x in t], [0, 1, 0])
        self.assertEqual([x["sent_back"] for x in t], [0, 1, 0])

    def test_numbers_flagged_though_on_a_page_read_in_an_earlier_turn(self):
        t = self.round()["turns"]
        self.assertEqual([x["numbers_flagged"] for x in t], [0, 2, 0])
        self.assertEqual([x["numbers_on_earlier_pages"] for x in t], [0, 1, 0])
        # the check got the flagged sentences and the text of the pages of the turns before, not of its own turn
        sentences, evidence = self.asked[0]
        self.assertEqual(sentences, ["La superficie era di 68000 km2 e ora è di 777 km2."])
        self.assertIn("68000", evidence)
        self.assertNotIn("diga", evidence)

    def test_without_the_check_or_the_cache_the_row_says_so(self):
        self.assertEqual([x["numbers_on_earlier_pages"] for x in self.round(still_missing=None)["turns"]], [None, None, None])
        r = fr.compute_round("c1", self.out, self.db, os.path.join(self.tmp.name, "nowhere"), fr.load_expected(self.expected),
                             still_missing=self.still_missing)
        self.assertEqual([x["numbers_on_earlier_pages"] for x in r["turns"]], [None, None, None])
        self.assertEqual([x["numbers_flagged"] for x in r["turns"]], [0, 2, 0])

    def test_notes_written_by_the_model(self):
        t = self.round()["turns"]
        self.assertEqual([x["notes_in_answer"] for x in t], [0, 0, 1])
        self.assertEqual([x["notes_in_draft"] for x in t], [0, 0, 1])

    def test_language_of_each_turn(self):
        t = self.round()["turns"]
        self.assertEqual([x["language_ok"] for x in t], [1, 1, 0])
        self.assertEqual([x["language_known"] for x in t], [1, 1, 1])
        self.assertEqual([x["language_known"] for x in self.round(languages={})["turns"]], [0, 0, 0])

    def test_no_seconds_without_the_turns_file(self):
        os.remove(os.path.join(self.out, "turns_c1.json"))
        self.assertEqual([x["seconds_median"] for x in self.round()["turns"]], [None, None, None])

    def test_table_by_turn(self):
        r = self.round()
        text = fr.format_turns([r])
        self.assertIn("c1", text)
        row = next(l for l in text.splitlines() if l.startswith("numbers flagged in the note"))
        self.assertEqual(row.split()[-3:], ["0", "2", "0"])
        self.assertIn("pages read again", text)
        fr.save_round(r, self.out)  # the rows by turn are saved with the round
        with open(os.path.join(self.out, "free_c1.json")) as f:
            self.assertEqual(len(json.load(f)["turns"]), 3)

    def test_a_round_of_single_answers_has_no_rows_by_turn(self):
        with open(os.path.join(self.out, "runs_c1.json"), "w") as f:
            json.dump({"single": "sid-s"}, f)
        r = self.round()
        self.assertEqual(r["turns"], [])
        self.assertEqual(fr.format_turns([r]), "")


if __name__ == "__main__":
    unittest.main()
