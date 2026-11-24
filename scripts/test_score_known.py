#!/usr/bin/env python3
"""Unit tests for scripts/score_known.py: the questions with a known answer judged on
the bench's rounds (Piano dopo audit 5.4). Stdlib only, no judge called.

Run: python3 -m unittest scripts/test_score_known.py
"""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import score_known as sk  # noqa: E402


class ScoreKnownTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.bench = os.path.join(self.dir, "benchmark.json")
        with open(self.bench, "w") as f:
            json.dump({"cases": [{"id": "history-fact-it", "known": "suez"}, {"id": "weather-3d"}],
                       "extra_cases": [{"id": "known-kohut", "known": "kohut"}]}, f)
        for name, runs in (("t_1", {"history-fact-it": "s1", "weather-3d": "s2", "known-kohut": "s3"}),
                           ("t_2", {"history-fact-it": "s4"})):
            with open(os.path.join(self.dir, f"runs_{name}.json"), "w") as f:
                json.dump(runs, f)

    def test_runs_from_tags(self):
        """A bench round becomes the runs score_known judges: the case's known entry,
        the round, the session; cases without a known answer are left out."""
        runs = sk.runs_from_tags(["t"], self.dir, self.bench)
        self.assertEqual(runs, [
            {"k": "suez", "v": "t_1", "round": "t_1", "sid": "s1"},
            {"k": "kohut", "v": "t_1", "round": "t_1", "sid": "s3"},
            {"k": "suez", "v": "t_2", "round": "t_2", "sid": "s4"},
        ])

    def test_runs_without_items_are_reported(self):
        """A case whose entry is not in the set yet (waiting for approval) is skipped
        and named, not a crash."""
        runs = sk.runs_from_tags(["t"], self.dir, self.bench)
        ready, missing = sk.split_known(runs, {"suez": {"must": [], "trap": []}})
        self.assertEqual([r["sid"] for r in ready], ["s1", "s4"])
        self.assertEqual(missing, ["kohut"])

    def test_summary(self):
        done = [
            {"round": "t_1", "kind": "must", "verdict": "RIGHT"},
            {"round": "t_1", "kind": "must", "verdict": "WRONG"},
            {"round": "t_1", "kind": "must", "verdict": "ABSENT"},
            {"round": "t_1", "kind": "must", "verdict": "RIGHT"},
            {"round": "t_1", "kind": "trap", "verdict": "MADE"},
            {"round": "t_1", "kind": "trap", "verdict": "AVOIDED"},
            {"round": "t_1", "kind": "must", "verdict": "ERROR"},
        ]
        s = sk.summary(done)["t_1"]
        self.assertEqual((s["must"], s["RIGHT"], s["WRONG"], s["ABSENT"]), (4, 2, 1, 1))
        self.assertEqual((s["trap"], s["MADE"], s["errors"]), (2, 1, 1))
        self.assertEqual(s["pct_right"], 50.0)
        self.assertIn("t_1", sk.format_summary(sk.summary(done)))

    def test_page_host(self):
        """An archived copy counts as a page of the site it copies."""
        self.assertEqual(sk.page_host("https://www.comune.roma.it/web/it/rubrica-pec.page"), "comune.roma.it")
        self.assertEqual(sk.page_host("https://web.archive.org/web/20210927163453/https://www.comune.roma.it/web/it/x.page"),
                         "comune.roma.it")
        self.assertEqual(sk.page_host("https://www.amministrazionicomunali.it/lazio/roma/pec"), "amministrazionicomunali.it")

    def test_addresses_on_pages(self):
        """A trap about e-mail addresses that no official page read shows is decided on
        the pages' text, which the judge never sees."""
        official = ("https://www.comune.roma.it/web/it/rubrica-pec.page",
                    "GABINETTO DEL SINDACO | protocollo.gabinettosindaco@pec.comune.roma.it")
        archived = ("https://web.archive.org/web/2021/https://www.comune.roma.it/web/it/rubrica-pec.page",
                    "DIPARTIMENTO MOBILITA' | protocollo.mobilitatrasporti@pec.comune.roma.it")
        other = ("https://www.amministrazionicomunali.it/lazio/roma/pec", "sindaco@comune.roma.it")
        answer = ("A: Protocollo.GabinettoSindaco@pec.comune.roma.it, "
                  "Cc: protocollo.mobilitatrasporti@pec.comune.roma.it.")
        hosts = ["comune.roma.it"]
        self.assertEqual(sk.addresses_on_pages(answer, [official, archived], hosts), ("AVOIDED", ""))
        self.assertEqual(sk.addresses_on_pages(answer, [official, other], hosts),
                         ("MADE", "protocollo.mobilitatrasporti@pec.comune.roma.it"))
        self.assertEqual(sk.addresses_on_pages("Scrivi al sindaco@comune.roma.it", [other], hosts),
                         ("MADE", "sindaco@comune.roma.it"))
        self.assertEqual(sk.addresses_on_pages("Nessun indirizzo qui.", [], hosts), ("AVOIDED", ""))
        # the writer's own address left to fill in is not an address of the site
        self.assertEqual(sk.addresses_on_pages("Mittente: nome.cognome@pec.it", [official], hosts), ("AVOIDED", ""))

    def test_session_pages(self):
        """The pages a session read are its successful reads, taken from the round's cache."""
        meta = {"loop": {"calls_detail": [
            {"tool": "search", "query": "pec comune roma"},
            {"tool": "read", "url": "https://www.comune.roma.it/web/it/rubrica-pec.page", "result": "ok"},
            {"tool": "read", "url": "https://example.org/gone", "result": "failed"},
            {"tool": "read", "url": "https://example.org/nocache", "result": "ok"},
        ]}}
        cache = {"comune.roma.it/web/it/rubrica-pec.page": {"content": "testo"}}
        self.assertEqual(sk.session_pages(meta, cache),
                         [("https://www.comune.roma.it/web/it/rubrica-pec.page", "testo")])

    def test_checked_items_skip_the_judge(self):
        """An item with a machine check gets no judge call; its verdict comes from the
        pages, and an old judge verdict on it is replaced."""
        import sqlite3
        db = sqlite3.connect(":memory:")
        db.execute("create table messages (session_id text, role text, content text, meta_json text, created_at text)")
        meta = {"loop": {"calls_detail": [{"tool": "read", "url": "https://www.comune.roma.it/pec", "result": "ok"}]}}
        db.execute("insert into messages values ('s1', 'assistant', 'Scrivi a a@pec.comune.roma.it e b@comune.roma.it, da io@pec.it', ?, '1')",
                   (json.dumps(meta),))
        check = {"type": "addresses_on_pages", "hosts": ["comune.roma.it"]}
        cases = {"roma": {"must": [{"t": "Sindaco Gualtieri", "e": "primary"}],
                          "trap": [{"t": "PEC non letta", "e": "machine", "check": check}]}}
        runs = [{"k": "roma", "v": "t_1", "round": "t_1", "sid": "s1"}]
        jobs = sk.build_jobs(runs, cases, db, set())
        self.assertEqual([(kind, item["t"]) for _, kind, item, _, _ in jobs], [("must", "Sindaco Gualtieri")])
        cache = {"comune.roma.it/pec": {"content": "a@pec.comune.roma.it"}}
        got = sk.machine_verdicts(runs, cases, db, lambda name: cache)
        self.assertEqual(len(got), 1)
        self.assertEqual((got[0]["item"], got[0]["verdict"], got[0]["quote"], got[0]["by"]),
                         ("PEC non letta", "MADE", "b@comune.roma.it", "machine"))
        self.assertEqual(sk.machine_verdicts(runs, cases, db, lambda name: None)[0]["verdict"], "ERROR")
        out = os.path.join(self.dir, "known_t_1.json")
        with open(out, "w") as f:
            json.dump([{"k": "roma", "v": "t_1", "round": "t_1", "sid": "s1", "kind": "must", "item": "Sindaco Gualtieri",
                        "evidence": "primary", "verdict": "RIGHT", "quote": "Gualtieri"},
                       {"k": "roma", "v": "t_1", "round": "t_1", "sid": "s1", "kind": "trap", "item": "PEC non letta",
                        "evidence": "machine", "verdict": "AVOIDED", "quote": ""}], f)
        done = sk.score(runs, cases, db, out, cfg="no judge needed", pages_of=lambda name: cache)
        self.assertEqual(sorted((d["item"], d["verdict"]) for d in done),
                         [("PEC non letta", "MADE"), ("Sindaco Gualtieri", "RIGHT")])

    def test_items_no_longer_in_the_set_are_dropped(self):
        """When an item's text changes, its old verdict leaves the totals and the new
        text is judged."""
        import sqlite3
        db = sqlite3.connect(":memory:")
        db.execute("create table messages (session_id text, role text, content text, meta_json text, created_at text)")
        db.execute("insert into messages values ('s1', 'assistant', 'risposta', '{}', '1')")
        cases = {"mel": {"must": [{"t": "efficace, 8 o 9 trial su 10", "e": "primary"}], "trap": []}}
        runs = [{"k": "mel", "v": "t_1", "round": "t_1", "sid": "s1"}]
        out = os.path.join(self.dir, "known_t_1.json")
        with open(out, "w") as f:
            json.dump([{"k": "mel", "v": "t_1", "round": "t_1", "sid": "s1", "kind": "must", "item": "efficace, 8 trial su 10",
                        "evidence": "primary", "verdict": "RIGHT", "quote": "8 of 10"}], f)
        asked = []
        orig = sk.ask
        sk.ask = lambda cfg, kind, item, body, pages: asked.append(item["t"]) or ("RIGHT", "x")
        try:
            done = sk.score(runs, cases, db, out, cfg="fake", pages_of=lambda name: None)
        finally:
            sk.ask = orig
        self.assertEqual(asked, ["efficace, 8 o 9 trial su 10"])
        self.assertEqual([d["item"] for d in done], ["efficace, 8 o 9 trial su 10"])

    def test_request_chars(self):
        """--estimate counts what a judging would send, without calling the judge."""
        item = {"t": "La swap sta su zram"}
        n = sk.request_chars("must", item, "risposta", "(none listed)")
        self.assertEqual(n, len(sk.SYSTEM) + len(sk.user_message("must", item, "risposta", "(none listed)")))
        self.assertIn("STATEMENT: La swap sta su zram", sk.user_message("must", item, "risposta", "(none listed)"))


if __name__ == "__main__":
    unittest.main()
