#!/usr/bin/env python3
"""Unit tests for scripts/eval.py (V25-T1.2). Stdlib only.

Run: python3 -m unittest scripts/test_eval.py
"""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import eval as ev  # noqa: E402

LONG = "Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore. " * 3

SOURCES = [
    {"url": "https://www.example.com/a/", "title": "A"},
    {"url": "https://docs.example.org/b", "title": "B"},
]

FACTS = [
    {"id": "F1", "claim": "Il biglietto costa 1.200 euro", "verbatim_snippet": "prezzo: 1200 €"},
    {"id": "F2", "claim": "Aperto nel 2008 con 4.000 animali", "verbatim_snippet": "aperto nel 2008, ospita 3.000 animali"},
    {"id": "F3", "claim": "Fatto mai citato", "verbatim_snippet": "testo"},
]


def session_with_meta():
    """Clean new-style session: meta present, tail after the body."""
    content = (
        "## Risposta\n\n"
        f"{LONG} Il prezzo è noto [1].\n\n"
        f"{LONG} Vedi anche [https://example.com/a].\n\n"
        "| tabella | senza citazioni |\n|---|---|\n\n"
        "---\n## Note Metodologiche\n\n"
        f"{LONG}\n\n"
        "\n\n> **Provenienza: grounded** — basata su fonti verificate."
        "\n\n---\n### Fonti e Riferimenti Verificati\n\n[1] **[A](https://www.example.com/a/)**\n\n[2] x [3] y\n"
    )
    return {
        "id": "s-meta",
        "messages": [{
            "content": content,
            "facts": FACTS,
            "sources": SOURCES,
            "meta": {"v": 1, "provenance": "grounded", "partial": False,
                     "citations": [{"n": 1, "fact_id": "F1", "source_url": "https://www.example.com/a/"}]},
        }],
    }


def legacy_session_en():
    """Old session without meta, English answer, phantom link, uncited paragraph."""
    content = (
        f"{LONG} Supported claim [1] and [2].\n\n"
        f"{LONG} This paragraph cites nothing at all.\n\n"
        f"{LONG} Invented link [https://evil.example.net/x].\n\n"
        "---\n## Methodological Notes\n\nnotes\n\n"
        "> **Provenance: grounded** — based on verified sources.\n\n"
        "---\n### Fonti e Riferimenti Verificati\n\n[1] a\n\n[2] b\n\n[3] c\n\n[4] d\n"
    )
    return {"id": "s-legacy", "messages": [{"content": content, "facts": FACTS, "sources": SOURCES}]}


def not_found_session():
    content = (
        "Non sono stati trovati fatti verificati nelle fonti consultate sul brevetto richiesto.\n\n"
        "> **Provenienza: parametric** — nessuna evidenza verificata."
    )
    return {"id": "s-nf", "messages": [{"content": content, "facts": [], "sources": SOURCES,
                                        "meta": {"v": 1, "provenance": "parametric", "citations": []}}]}


class HelpersTest(unittest.TestCase):
    def test_canonical_numbers(self):
        self.assertEqual(ev.canonical_numbers("1.000 e 1,000 e 1000"), ["1000", "1000", "1000"])
        self.assertEqual(ev.canonical_numbers("12,5% contro 12.5 %"), ["12.5", "12.5"])
        self.assertEqual(ev.canonical_numbers("dal 2019-2021"), ["2019", "2021"])

    def test_norm_url(self):
        self.assertEqual(ev.norm_url("https://www.Example.com/a/"), ev.norm_url("http://example.com/a"))
        self.assertEqual(ev.norm_url("https://example.com/a#frag)."), "example.com/a")

    def test_answer_body_cuts_tail_in_every_language(self):
        for heading in ["## Note Metodologiche", "## Methodological Notes", "## Methodische Anmerkungen",
                        "## Notes Méthodologiques", "## Notas Metodológicas e Controversias",
                        "### Fonti e Riferimenti Verificati", "> **Herkunft: grounded**",
                        "<!-- vedetta:notes -->"]:
            body = ev.answer_body("Testo della risposta [1].\n\n---\n" + heading + "\n\ncoda [9]")
            self.assertEqual(body, "Testo della risposta [1].", heading)

    def test_horizontal_rule_inside_answer_is_kept(self):
        content = "Prima parte [1].\n\n---\n\n**Sintesi** [2]\n\n## Note Metodologiche\n\ncoda"
        self.assertIn("**Sintesi** [2]", ev.answer_body(content))


class SignalsTest(unittest.TestCase):
    def test_meta_session(self):
        sig = ev.extract_signals(session_with_meta())
        self.assertTrue(sig["has_meta"])
        self.assertEqual(sig["provenance"], "grounded")
        self.assertEqual(sig["citations"], 1)  # from meta, not the bibliography numbers
        self.assertAlmostEqual(sig["cited_fact_ratio"], 1 / 3)
        self.assertEqual(sig["paragraphs"], 2)  # table and tail excluded
        self.assertEqual(sig["uncited_paragraphs"], [])
        self.assertEqual(sig["links_outside"], [])  # www./trailing slash normalized
        self.assertEqual(sig["numbers_checked"], 1)  # only the cited fact F1
        self.assertEqual(sig["number_mismatch"], [])  # 1.200 == 1200

    def test_legacy_session_text_fallback(self):
        sig = ev.extract_signals(legacy_session_en())
        self.assertFalse(sig["has_meta"])
        self.assertEqual(sig["provenance"], "grounded")  # English label parsed
        self.assertEqual(sig["citations"], 2)  # body only, bibliography [3] [4] ignored
        self.assertEqual(len(sig["uncited_paragraphs"]), 1)
        self.assertEqual(sig["links_outside"], ["evil.example.net/x"])
        self.assertEqual(sig["numbers_checked"], 3)  # no citation map: all facts
        self.assertEqual([m[0] for m in sig["number_mismatch"]], ["F2"])  # 4000 not in snippet
        self.assertEqual(sig["number_mismatch"][0][1], ["4000"])


class ChecksTest(unittest.TestCase):
    def check(self, chk, session):
        return ev.run_check(chk, ev.extract_signals(session))

    def test_new_checks_pass_on_clean_session(self):
        s = session_with_meta()
        self.assertTrue(self.check({"type": "links_in_sources"}, s)[0])
        self.assertTrue(self.check({"type": "uncited_paragraph_ratio_max", "value": 0.3}, s)[0])
        self.assertTrue(self.check({"type": "claim_numbers_in_snippet"}, s)[0])
        self.assertFalse(self.check({"type": "cited_fact_ratio_min", "value": 0.5}, s)[0])
        self.assertTrue(self.check({"type": "provenance_in", "values": ["grounded"]}, s)[0])
        # the Deep a ciclo says "researched" (05/11/2026), and the legacy text is parsed too
        self.assertEqual(ev.PROVENANCE_TEXT.search("> **Provenienza: researched** — ricerca").group(1), "researched")
        self.assertTrue(self.check({"type": "min_citations", "value": 1}, s)[0])

    def test_new_checks_fail_on_problematic_session(self):
        s = legacy_session_en()
        self.assertFalse(self.check({"type": "links_in_sources"}, s)[0])
        self.assertFalse(self.check({"type": "uncited_paragraph_ratio_max", "value": 0.3}, s)[0])
        self.assertFalse(self.check({"type": "claim_numbers_in_snippet"}, s)[0])
        self.assertTrue(self.check({"type": "claim_numbers_in_snippet", "value": 0.5}, s)[0])

    def test_not_found(self):
        self.assertTrue(self.check({"type": "answer_declares_not_found"}, not_found_session())[0])
        self.assertFalse(self.check({"type": "answer_declares_not_found"}, session_with_meta())[0])
        text_only = {"id": "x", "messages": [{"content": "Non ho trovato evidenze affidabili su questo tema.",
                                              "facts": [], "sources": []}]}
        self.assertTrue(self.check({"type": "answer_declares_not_found"}, text_only)[0])

    def test_not_found_phrasings(self):
        for text in ["Non esiste alcuna normativa denominata così.",
                     "No entity named \"Acme Ltd\" appears in the registries.",
                     "The company does not appear in any registry consulted."]:
            s = {"id": "x", "messages": [{"content": text, "facts": [], "sources": []}]}
            self.assertTrue(self.check({"type": "answer_declares_not_found"}, s)[0], text)

    def test_link_with_tracking_query_is_inside(self):
        s = session_with_meta()
        s["messages"][0]["sources"].append({"url": "https://shop.example.com/p?srsltid=abc", "title": "P"})
        s["messages"][0]["content"] = s["messages"][0]["content"].replace(
            "Vedi anche [https://example.com/a].", "Vedi anche [https://example.com/a] e [https://shop.example.com/p].")
        self.assertTrue(self.check({"type": "links_in_sources"}, s)[0])

    def test_skips(self):
        s = not_found_session()
        self.assertIsNone(self.check({"type": "uncited_paragraph_ratio_max", "value": 0.3}, s)[0])
        self.assertIsNone(self.check({"type": "claim_numbers_in_snippet"}, s)[0])
        self.assertFalse(self.check({"type": "cited_fact_ratio_min", "value": 0.5}, s)[0])

    def test_no_facts_honest(self):
        # V25-QA A2: an answer without facts must open with Go's sentence, links inside sources
        good = {"id": "x", "messages": [{"content": ev.NO_FACTS_LEADS[0] + "\n\nLe fonti non coprono la domanda.",
                                         "facts": [], "sources": SOURCES,
                                         "meta": {"v": 1, "provenance": "parametric", "citations": []}}]}
        self.assertTrue(self.check({"type": "no_facts_honest"}, good)[0])
        self.assertFalse(self.check({"type": "no_facts_honest"}, not_found_session())[0])
        bad_link = {"id": "x", "messages": [{"content": ev.NO_FACTS_LEADS[1] + "\n\nSee [https://evil.example.net/x].",
                                             "facts": [], "sources": SOURCES}]}
        self.assertFalse(self.check({"type": "no_facts_honest"}, bad_link)[0])
        self.assertIsNone(self.check({"type": "no_facts_honest"}, session_with_meta())[0])

    def test_answer_contains_any_ignores_tail(self):
        s = session_with_meta()  # "grounded" appears only in the provenance tail
        self.assertFalse(self.check({"type": "answer_contains_any", "values": ["grounded"]}, s)[0])
        self.assertTrue(self.check({"type": "answer_contains_any", "values": ["PREZZO"]}, s)[0])

    def test_benchmark_file_is_consistent(self):
        with open(ev.DEFAULT_BENCHMARK) as f:
            bench = ev.json.load(f)
        every = bench["cases"] + bench.get("extra_cases", [])
        ids = [c["id"] for c in every]
        self.assertEqual(len(ids), len(set(ids)))
        sig = ev.extract_signals(session_with_meta())
        for c in every:
            for chk in c["checks"]:
                self.assertNotIn("unknown check type", ev.run_check(chk, sig)[1], c["id"])
        for suite in bench.get("suites", {}).values():
            self.assertTrue(set(suite) <= set(ids))

    def test_extra_cases_run_only_through_a_suite(self):
        """Cases outside the 19 of the normal round (Piano dopo audit 5.4): the round
        without a suite stays the same, a suite can name them."""
        bench = {"cases": [{"id": "a"}, {"id": "b"}], "extra_cases": [{"id": "x"}],
                 "suites": {"s": ["a", "x"]}}
        selected, cases = ev.select_cases(bench, None)
        self.assertEqual(selected, ["a", "b"])
        self.assertEqual(set(cases), {"a", "b", "x"})
        selected, _ = ev.select_cases(bench, "s")
        self.assertEqual(selected, ["a", "x"])
        with self.assertRaises(SystemExit):
            ev.select_cases(bench, "missing")

    def test_known_suite(self):
        """The suite of the questions with a known answer (Piano dopo audit 5.4): every
        case in it names its entry in docs/domande_risposta_nota.json, and the normal
        round keeps its 19 cases."""
        with open(ev.DEFAULT_BENCHMARK) as f:
            bench = ev.json.load(f)
        self.assertEqual(len(bench["cases"]), 19)
        every = {c["id"]: c for c in bench["cases"] + bench.get("extra_cases", [])}
        known = bench["suites"]["known"]
        self.assertEqual(len(known), 19)
        names = [every[cid].get("known") for cid in known]
        self.assertTrue(all(names), [cid for cid in known if not every[cid].get("known")])
        self.assertEqual(len(names), len(set(names)))
        for c in bench.get("extra_cases", []):
            self.assertEqual(c.get("mode"), "deep", c["id"])
            # real questions of the user stay in docs/ (local): the versioned file has the link only
            self.assertNotIn("query", c, c["id"])

    def test_known_questions_come_from_the_local_set(self):
        """An extra case gets its question from the set by its known entry; without the
        set, or without the entry, the case cannot run and says why."""
        import json
        import tempfile
        path = os.path.join(tempfile.mkdtemp(), "set.json")
        with open(path, "w") as f:
            json.dump({"cases": [{"id": "kohut", "q": "Chi era Kohut?"}]}, f)
        bench = {"cases": [{"id": "a", "query": "q"}],
                 "extra_cases": [{"id": "known-kohut", "known": "kohut"}, {"id": "known-x", "known": "x"}]}
        ev.fill_known_queries(bench, path)
        self.assertEqual(bench["extra_cases"][0]["query"], "Chi era Kohut?")
        self.assertNotIn("query", bench["extra_cases"][1])
        self.assertEqual(bench["cases"][0]["query"], "q")
        ev.fill_known_queries(bench, os.path.join(os.path.dirname(path), "missing.json"))  # no set: no crash
        with self.assertRaises(SystemExit) as cm:
            ev.check_runnable({"id": "known-x", "known": "x"})
        self.assertIn("domande_risposta_nota", str(cm.exception))

    def test_report_renders(self):
        sig = ev.extract_signals(legacy_session_en())
        case = {"id": "t", "checks": [{"type": "links_in_sources"}]}
        _, results = ev.evaluate(case, legacy_session_en())
        passed, total, text = ev.render_report(case, legacy_session_en(), sig, results)
        self.assertEqual((passed, total), (0, 1))
        self.assertIn("honesty:", text)
        self.assertIn("meta=no (text fallback)", text)

    def test_cache_line_and_summary(self):
        case = {"id": "x", "checks": []}
        sess = session_with_meta()
        sess["messages"][-1]["meta"].update({"v": 4, "cache": {
            "mode": "replay", "search_hits": 3, "search_misses": 1, "page_hits": 4, "page_misses": 0}})
        sig = ev.extract_signals(sess)
        _, _, text = ev.render_report(case, sess, sig, [])
        self.assertIn("cache: mode=replay searches=3/4 pages=4/4", text)

        no_cache = ev.extract_signals(session_with_meta())
        self.assertIsNone(no_cache["cache"])
        self.assertIsNone(ev.cache_summary([no_cache]))
        summary = ev.cache_summary([sig, sig, no_cache])
        self.assertIn("replay - searches 75% hit (6/8), pages 100% hit (8/8)", summary)
        self.assertIn("[1 sessions without cache data]", summary)

    def test_search_conditions_line(self):
        sess = session_with_meta()
        sess["messages"][-1]["meta"].update({"v": 3, "search_queries": 5, "empty_searches": 1,
                                             "unresponsive_engines": [{"engine": "google cse", "count": 3, "reason": "too many requests"}]})
        sig = ev.extract_signals(sess)
        case = {"id": "t", "checks": []}
        _, results = ev.evaluate(case, sess)
        _, _, text = ev.render_report(case, sess, sig, results)
        self.assertIn("search: queries=5 empty=1 unresponsive=[google cse (too many requests x3)]", text)
        # older meta: no search line
        _, _, old = ev.render_report(case, session_with_meta(), ev.extract_signals(session_with_meta()), results)
        self.assertNotIn("search:", old)


class FakeStream:
    """What urlopen gives back for /api/search: the lines of the SSE stream."""

    def __init__(self, lines):
        self.lines = [l.encode() for l in lines]

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def __iter__(self):
        return iter(self.lines)


class ConversationsTest(unittest.TestCase):
    """Piano dopo audit 5.3: a case with "turns" is one conversation, sent turn after turn
    in the same session."""

    CASE = {"id": "conv-x", "mode": "deep", "answer_language": "it", "turns": ["uno", "due", "tre"],
            "checks": [{"type": "completes"}]}

    def with_fake_query(self, fn):
        sent = []

        def fake(base, case, timeout=None, session_id=None):
            sent.append((case["query"], session_id))
            return "sid-1"
        orig, ev.run_query = ev.run_query, fake
        try:
            return fn(), sent
        finally:
            ev.run_query = orig

    def test_turns_go_one_after_another_in_the_same_session(self):
        (sid, turns), sent = self.with_fake_query(lambda: ev.run_case("http://x", self.CASE))
        self.assertEqual(sid, "sid-1")
        self.assertEqual(sent, [("uno", None), ("due", "sid-1"), ("tre", "sid-1")])
        self.assertEqual([t["q"] for t in turns], ["uno", "due", "tre"])
        self.assertTrue(all(isinstance(t["seconds"], int) for t in turns))

    def test_a_case_without_turns_is_one_question(self):
        (sid, turns), sent = self.with_fake_query(lambda: ev.run_case("http://x", {"id": "a", "query": "sola"}))
        self.assertEqual(sent, [("sola", None)])
        self.assertEqual(len(turns), 1)

    def test_a_follow_up_names_its_session_in_the_request(self):
        import json
        import urllib.request
        bodies = []

        def fake_urlopen(req, timeout=None):
            bodies.append(json.loads(req.data.decode()))
            return FakeStream(["event: status", 'data: {"step": "search"}'])  # a follow-up stream may not repeat the id
        orig, urllib.request.urlopen = urllib.request.urlopen, fake_urlopen
        try:
            sid = ev.run_query("http://x", {"query": "due", "mode": "deep"}, session_id="sid-9")
            with self.assertRaises(RuntimeError):
                ev.run_query("http://x", {"query": "uno", "mode": "deep"})  # a first question must get an id
        finally:
            urllib.request.urlopen = orig
        self.assertEqual(sid, "sid-9")
        self.assertEqual(bodies[0]["session_id"], "sid-9")
        self.assertNotIn("session_id", bodies[1])

    def test_a_conversation_needs_no_single_question(self):
        ev.check_runnable(self.CASE)  # no exit
        with self.assertRaises(SystemExit):
            ev.check_runnable({"id": "conv-empty", "turns": []})

    def test_every_turn_must_have_an_answer(self):
        def session(n):
            return {"id": "s", "messages": [{"role": "assistant", "content": f"risposta {i}", "meta": {}} for i in range(n)]}
        _, results = ev.evaluate(self.CASE, session(3))
        self.assertIn(("turns_answered", True, "3/3 turns answered"), results)
        _, results = ev.evaluate(self.CASE, session(2))
        self.assertIn(("turns_answered", False, "2/3 turns answered"), results)
        sig, _ = ev.evaluate(self.CASE, session(3))
        self.assertEqual(sig["content"], "risposta 2")  # the signals of a conversation are those of its last answer
        _, single = ev.evaluate({"id": "a", "checks": []}, session(1))
        self.assertNotIn("turns_answered", [name for name, _, _ in single])

    def test_conv_suite(self):
        """Six fixed conversations of three turns (docs/PROPOSTA_5_3_CONVERSAZIONI.md, approved
        13/11/2026): outside the normal round, short first question."""
        with open(ev.DEFAULT_BENCHMARK) as f:
            bench = ev.json.load(f)
        selected, cases = ev.select_cases(bench, "conv")
        self.assertEqual(len(selected), 6)
        for cid in selected:
            c = cases[cid]
            self.assertEqual((len(c["turns"]), c["mode"], c["answer_language"]), (3, "deep", "it"), cid)
            self.assertLess(len(c["turns"][0]), 60, cid)
        self.assertFalse(set(selected) & {c["id"] for c in bench["cases"]})


if __name__ == "__main__":
    unittest.main()
