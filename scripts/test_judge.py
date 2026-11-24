#!/usr/bin/env python3
"""Unit tests for scripts/judge.py (V25-T1.3). Stdlib only, no network.

Run: python3 -m unittest scripts/test_judge.py
"""
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import judge as j  # noqa: E402

FACTS = [
    {"id": "F1", "claim": "c1", "verbatim_snippet": "Construction began on January 26, 1887."},
    {"id": "F2", "claim": "c2", "verbatim_snippet": "Current height | 1083 feet"},
]
SOURCES = [{"url": "https://www.example.com/page/", "title": "Example", "content": "The tower is 330 m tall."}]

BODY = (
    "## Storia\n\n"
    "La costruzione iniziò il 26 gennaio 1887 [1]. Questo valore è citato due volte [2]. "
    "Altezza confermata da una fonte esterna [[https://example.com/page]].\n\n"
    "- Frase in elenco con citazione inesistente [7].\n"
    "- Breve [1].\n\n"
    "Una frase lunga senza nessuna citazione che afferma un dettaglio tecnico preciso.\n\n"
    "| Domanda | Risposta |\n|---|---|\n| Altezza | 330 m [2] |\n"
)
TAIL = "\n\n---\n## Note Metodologiche\n\nnote [1]\n\n> **Provenienza: grounded** — x\n"


def turn_with_meta():
    return {"content": BODY + TAIL, "facts": FACTS, "sources": SOURCES,
            "meta": {"citations": [{"n": 1, "fact_id": "F1", "source_url": "u1"},
                                   {"n": 2, "fact_id": "F2", "source_url": "u2"}]}}


def legacy_turn():
    bib = ("\n\n---\n### Fonti e Riferimenti Verificati\n\n"
           "[1] **[T1](https://a.example/1)**\n> *«Construction began on January 26, 1887.»*\n\n"
           "[2] **[T2](https://a.example/2)**\n> *«Current height | 1083 feet»*\n\n")
    return {"content": BODY + TAIL + bib, "facts": FACTS, "sources": SOURCES}


class ParsingTest(unittest.TestCase):
    def test_split_sentences(self):
        sents = j.split_sentences("# T\n\nUna. Due [1]. tre minuscola.\n- punto\n| a | b |\n|---|---|\n---")
        self.assertEqual(sents, ["Una.", "Due [1]. tre minuscola.", "punto", "a — b"])

    def test_clean_sentence(self):
        s = j.clean_sentence("Il **valore** è 3 [1][2] secondo [[https://x.org/a]] e [link](https://y.org) [[fact:F9]].")
        self.assertEqual(s, "Il valore è 3 secondo e link .")

    def test_snippet_citations(self):
        # V25-T3.5: [Sn] points to the preview quoted in the snippet bibliography.
        turn = {
            "content": "## Risposta\n\nIl MiTE ha pubblicato linee guida nel 2022 [S1].\n\n"
                       "---\n### Anteprime di Ricerca Citate (non lette per intero)\n\n"
                       "[S1] **[Linee guida](https://site.it/2022/07/28/linee/)**\n> *«Linee guida MiTE 2022»*\n",
            "sources": [{"url": "https://site.it/2022/07/28/linee/", "title": "Linee guida", "content": "x"}],
            "meta": {"citations": [], "snippet_citations": [{"n": 1, "snippet_id": "S3",
                                                             "url": "https://site.it/2022/07/28/linee/"}]},
        }
        items = j.build_items(turn)
        self.assertEqual(len(items), 1)
        self.assertEqual(items[0]["kind"], "snippet")
        self.assertEqual(items[0]["sentence"], "Il MiTE ha pubblicato linee guida nel 2022 .")
        self.assertEqual(items[0]["excerpts"][0]["text"], "Linee guida MiTE 2022")
        self.assertEqual(items[0]["missing"], [])

    def test_parse_verdict(self):
        self.assertEqual(j.parse_verdict("UNSUPPORTED"), "UNSUPPORTED")
        self.assertEqual(j.parse_verdict("supported."), "SUPPORTED")
        self.assertEqual(j.parse_verdict("Verdict: PARTIAL"), "PARTIAL")
        self.assertEqual(j.parse_verdict("non so"), "ERROR")

    def check_items(self, items, expect_fact_text):
        by_sentence = {it["sentence"]: it for it in items}
        first = by_sentence["La costruzione iniziò il 26 gennaio 1887 ."]
        self.assertEqual(first["kind"], "fact")
        self.assertEqual(first["excerpts"][0]["text"], expect_fact_text)
        second = by_sentence["Questo valore è citato due volte ."]
        self.assertEqual(second["context"], "La costruzione iniziò il 26 gennaio 1887 .")
        url = by_sentence["Altezza confermata da una fonte esterna ."]
        self.assertEqual(url["kind"], "snippet")
        self.assertIn("330 m", url["excerpts"][0]["text"])  # www./slash normalized
        phantom = by_sentence["Frase in elenco con citazione inesistente ."]
        self.assertEqual((phantom["excerpts"], phantom["missing"]), ([], ["[7]"]))
        self.assertNotIn("Breve .", by_sentence)  # too short to judge
        uncited = [it for it in items if it["kind"] == "uncited"]
        self.assertEqual(len(uncited), 1)
        self.assertIn("Altezza — 330 m", by_sentence)  # table row
        self.assertFalse(any("note" in s for s in by_sentence))  # tail excluded

    def test_items_from_meta(self):
        self.check_items(j.build_items(turn_with_meta(), claims=False), "Construction began on January 26, 1887.")

    def test_items_from_legacy_bibliography(self):
        self.check_items(j.build_items(legacy_turn(), claims=False), "Construction began on January 26, 1887.")


class EvidenceBasisTest(unittest.TestCase):
    def test_passage_preferred(self):
        turn = turn_with_meta()
        turn["facts"] = [dict(FACTS[0], passage="Construction began on January 26, 1887, and lasted two years."),
                         dict(FACTS[1], evidence="Current height | 1083 feet (330 m)")]
        items = j.build_items(turn)
        first = next(i for i in items if i["sentence"].startswith("La costruzione"))
        self.assertIn("lasted two years", first["excerpts"][0]["text"])
        self.assertEqual(first["excerpts"][0]["basis"], "passage")
        self.assertEqual(j.evidence_basis(items), "evidence=2, passage=1")

    def test_legacy_snippet_basis(self):
        self.assertEqual(j.evidence_basis(j.build_items(turn_with_meta())), "verbatim_snippet=3")


class JudgeTest(unittest.TestCase):
    def test_judge_and_summary(self):
        items = j.build_items(turn_with_meta(), claims=False)
        asked = []

        def fake_ask(item):
            asked.append(item["sentence"])
            return "PARTIAL" if "330" in item["sentence"] else "SUPPORTED"

        items = j.judge_items(items, fake_ask, workers=2)
        verdicts = {it["sentence"]: it["verdict"] for it in items}
        self.assertEqual(verdicts["Frase in elenco con citazione inesistente ."], "NO_EVIDENCE")
        self.assertEqual(verdicts["Altezza — 330 m"], "PARTIAL")
        self.assertNotIn("Frase in elenco con citazione inesistente .", asked)
        self.assertFalse(any(it["kind"] == "uncited" for it in items if it["sentence"] in asked))

        summary = j.summarize(items)["all"]
        self.assertEqual(summary["judged"], 4)
        self.assertEqual((summary["SUPPORTED"], summary["PARTIAL"]), (3, 1))
        self.assertEqual((summary["NO_EVIDENCE"], summary["UNCITED"]), (1, 1))
        self.assertEqual(summary["supported_pct"], 75.0)

        text = j.render("sid", "title", {"model": "m", "base_url": "http://h/v1", "effort": "low",
                                         "source": "/x/judge.env"}, items, j.summarize(items))
        self.assertIn("% SUPPORTED: 75.0", text)
        self.assertIn("UNCITED (no citation, not judged) (1)", text)


class DeclaredDeductionTest(unittest.TestCase):
    """V25-QA A5: sentences opened by Go's deduction label are listed, never judged."""

    def test_declared_not_judged(self):
        turn = turn_with_meta()
        turn["content"] = turn["content"].replace(
            "Breve [1].",
            "Breve [1].\n\n- *Deduzione, non scritta nelle fonti:* Serve un ponte nel locale per la stampante [1].\n\n"
            "*Inference, not stated in the sources:* A cloud service alone cannot reach a printer on a private network.\n\n"
            "- [ ] *Deduzione, non scritta nelle fonti:* Fissare il prezzo dopo aver sentito tre locali della zona.",
            1)
        items = j.build_items(turn)
        declared = [it for it in items if it["kind"] == "declared"]
        self.assertEqual(len(declared), 3)  # the checklist box too
        self.assertTrue(all(not it["excerpts"] for it in declared))

        asked = []
        items = j.judge_items(items, lambda it: asked.append(it["sentence"]) or "SUPPORTED")
        self.assertFalse(any("ponte" in s or "cloud" in s for s in asked))
        summary = j.summarize(items)["all"]
        self.assertEqual(summary["DECLARED"], 3)
        text = j.render("sid", "title", {"model": "m", "base_url": "http://h/v1", "effort": "low",
                                         "source": "/x/judge.env"}, items, j.summarize(items))
        self.assertIn("declared deductions (not judged): 3", text)
        self.assertIn("DECLARED (deduction declared by the writer, not judged) (3)", text)


class AnchorTest(unittest.TestCase):
    """Deep a ciclo: a link to a page read is judged on Go's anchored passage."""

    def turn(self):
        return {
            "content": "## Novità\n\n**DockTail** espone i container come Tailscale Services "
                       "([selfh.st](https://selfh.st/weekly/2026-10-23/)). "
                       "Homarr 2.0 ha un assistente MCP ([homarr](https://homarr.dev/blog/x)). "
                       "Glance è leggero ([anteprima](https://other.test/g)).\n",
            "sources": [{"url": "https://selfh.st/weekly/2026-10-23/", "title": "Self-Host Weekly", "content": "short preview"},
                        {"url": "https://other.test/g", "title": "Glance", "content": "Glance is a lightweight dashboard"}],
            "meta": {"anchors": [
                {"sentence": "**DockTail** espone i container come Tailscale Services ([selfh.st]",
                 "url": "https://selfh.st/weekly/2026-10-23/", "passage": "DockTail exposes containers as Tailscale Services", "basis": "page"},
                {"sentence": "Homarr 2.0 ha un assistente MCP ([homarr]",
                 "url": "https://homarr.dev/blog/x", "passage": "Homarr 2.0 adds an AI assistant with MCP", "basis": "verified"},
                {"sentence": "Un'altra frase sulla stessa pagina", "url": "https://selfh.st/weekly/2026-10-23/",
                 "passage": "unrelated passage", "basis": "page"}]},
        }

    def test_anchor_used(self):
        items = {i["sentence"][:12]: i for i in j.build_items(self.turn())}
        dock = items["DockTail esp"]
        self.assertEqual(dock["kind"], "page")
        self.assertEqual(dock["excerpts"][0]["text"], "DockTail exposes containers as Tailscale Services")
        self.assertEqual(items["Homarr 2.0 h"]["excerpts"][0]["basis"], "verified")
        glance = items["Glance è leg"]
        self.assertEqual(glance["kind"], "snippet")
        self.assertIn("lightweight dashboard", glance["excerpts"][0]["text"])

    def test_preview_mode(self):
        items = j.build_items(self.turn(), use_anchors=False)
        dock = items[0]
        self.assertEqual(dock["kind"], "snippet")
        self.assertIn("short preview", dock["excerpts"][0]["text"])
        self.assertEqual(items[1]["verdict"] if "verdict" in items[1] else items[1]["missing"], ["https://homarr.dev/blog/x"])

    def test_summary_has_page_bucket(self):
        items = j.build_items(self.turn())
        for it in items:
            it["verdict"] = "SUPPORTED"
        self.assertEqual(j.summarize(items)["page"]["judged"], 2)


class EveryClaimTest(unittest.TestCase):
    def test_pipeline_claims_use_closest_fact(self):
        items = j.build_items(turn_with_meta())
        claim = [i for i in items if i["kind"] == "claim"]
        self.assertEqual(len(claim), 1)
        self.assertEqual(claim[0]["verdict"] if "verdict" in claim[0] else None, None)
        off = j.build_items(turn_with_meta(), claims=False)
        self.assertEqual([i["kind"] for i in off if not i["excerpts"] and i["kind"] == "uncited"], ["uncited"])

    def test_loop_claims_use_go_anchor(self):
        turn = {"content": "## A\n\nPatchMon è una piattaforma self-hosted di patch management per flotte Linux.\n"
                           "Una frase lunga che non ha nessun ancoraggio salvato da Go nei metadati.\n",
                "meta": {"loop": {"calls": 3}, "anchors": [
                    {"sentence": "PatchMon è una piattaforma self-hosted di patch management per flotte Linux.",
                     "url": "https://selfh.st/w", "passage": "PatchMon is a patch management platform", "basis": "page", "linked": False}]}}
        items = j.judge_items(j.build_items(turn), lambda it: "SUPPORTED")
        self.assertEqual([i["verdict"] for i in items], ["SUPPORTED", "NOTHING_CLOSE"])
        s = j.summarize(items)
        self.assertEqual(s["every_claim"]["judged"], 2)
        self.assertEqual(s["every_claim"]["supported_pct"], 50.0)
        self.assertEqual(s["all"]["judged"], 0)


class SecondPassTest(unittest.TestCase):
    def test_paragraph_and_second_pass(self):
        turn = {"content": "## A\n\n- **Arcane** è una UI Docker alternativa a Portainer con scansione Trivy.\n"
                           "  → [github.com/getarcaneapp/arcane](https://github.com/getarcaneapp/arcane)\n"
                           "- Altro elemento lungo abbastanza per essere una affermazione da giudicare.\n",
                "sources": [{"url": "https://github.com/getarcaneapp/arcane", "title": "arcane", "content": "Modern Docker management"}],
                "meta": {"loop": {"calls": 2}, "anchors": [
                    {"sentence": "Arcane è una UI Docker alternativa a Portainer con scansione Trivy.", "linked": False,
                     "url": "https://bitdoze.com/arcane", "basis": "page",
                     "passage": "Arcane is a Docker UI, an alternative to Portainer, with Trivy scans"}]}}
        items = j.build_items(turn)
        arcane = items[0]
        self.assertEqual(arcane["kind"], "claim")  # its link is on the next line of the same item
        self.assertEqual([e["ref"] for e in arcane["excerpts"]], ["https://github.com/getarcaneapp/arcane", "closest"])

        def ask(it):
            texts = " ".join(e["text"] for e in it["excerpts"])
            return "SUPPORTED" if "Trivy" in texts else "PARTIAL"
        cited = {"sentence": "Arcane ha la scansione Trivy.", "context": "", "kind": "snippet", "missing": [],
                 "excerpts": [{"ref": "u", "kind": "snippet", "text": "Modern Docker management"}],
                 "extra": [{"ref": "closest", "kind": "closest", "text": "Arcane ... Trivy scans"}]}
        out = j.judge_items([cited], ask)
        self.assertEqual((out[0]["verdict"], out[0]["claim_verdict"]), ("PARTIAL", "SUPPORTED"))
        s = j.summarize(out)
        self.assertEqual((s["all"]["supported_pct"], s["every_claim"]["supported_pct"]), (0.0, 100.0))


class ConfigTest(unittest.TestCase):
    def setUp(self):
        self.saved = {k: os.environ.pop(k) for k in list(os.environ) if k.startswith("JUDGE_")}

    def tearDown(self):
        for k in [k for k in os.environ if k.startswith("JUDGE_")]:
            del os.environ[k]
        os.environ.update(self.saved)

    def test_env_file(self):
        with tempfile.NamedTemporaryFile("w", suffix=".env", delete=False) as f:
            f.write("# comment\nJUDGE_BASE_URL=https://api.example/v1/\nJUDGE_MODEL='m-1'\nJUDGE_API_KEY=k\n")
        try:
            cfg = j.judge_config(f.name, "http://unused")
        finally:
            os.unlink(f.name)
        self.assertEqual((cfg["base_url"], cfg["model"], cfg["api_key"], cfg["effort"]),
                         ("https://api.example/v1", "m-1", "k", "low"))

    def test_environment_wins(self):
        os.environ.update({"JUDGE_BASE_URL": "http://env/v1", "JUDGE_MODEL": "env-model",
                           "JUDGE_REASONING_EFFORT": "none"})
        cfg = j.judge_config("/nonexistent.env", "http://unused")
        self.assertEqual((cfg["model"], cfg["effort"], cfg["source"]), ("env-model", "none", "environment"))


if __name__ == "__main__":
    unittest.main()
