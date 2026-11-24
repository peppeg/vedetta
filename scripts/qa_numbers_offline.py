#!/usr/bin/env python3
"""Offline test of the A1 number check (docs/SPEC_QA_PACCHETTO_ONESTA.md §2, A1.1).

Reads the facts saved by benchmark rounds (bench DB, read-only), runs the Go
check (scripts/claimnumbers) with evidence = Evidence + Passage, and writes
scripts/out/qa_numeri_offline.md: discards per round and case, and a fixed
random sample of 50 discards to read by hand (right or false alarm).

    python3 scripts/qa_numbers_offline.py m1Hpdf m1Hbj m1C R0 R0rep
"""
import json
import os
import random
import sqlite3
import subprocess
import sys
from collections import Counter, defaultdict

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "scripts", "out")
BENCH_DB = os.path.expanduser("~/vedetta-bench/data/vedetta.db")
SAMPLE = 50


def load_facts(tags):
    questions = {c["id"]: c["query"] for c in json.load(open(os.path.join(ROOT, "scripts", "benchmark.json")))["cases"]}
    db = sqlite3.connect("file:%s?mode=ro" % BENCH_DB, uri=True)
    db.text_factory = lambda b: b.decode("utf-8", "replace")
    facts = []
    for tag in tags:
        for n in (1, 2):
            path = os.path.join(OUT, "runs_%s_%d.json" % (tag, n))
            if not os.path.exists(path):
                continue
            rnd = "%s_%d" % (tag, n)
            for case, sid in json.load(open(path)).items():
                row = db.execute("select facts_json from messages where session_id=? and role='assistant' "
                                 "order by created_at desc limit 1", (sid,)).fetchone()
                for f in json.loads(row[0]) if row and row[0] else []:
                    facts.append({"round": rnd, "case": case, "question": questions.get(case, ""), "fid": f.get("id"), "claim": f.get("claim") or "",
                                  "evidence": f.get("evidence") or "", "passage": f.get("passage") or "",
                                  "snippet": f.get("verbatim_snippet") or "", "url": f.get("source_url") or ""})
    db.close()
    return facts


def run_go(facts):
    lines = "".join(json.dumps({"id": str(i), "claim": f["claim"], "evidence": f["evidence"] + "\n" + f["passage"],
                                "question": f["question"]},
                               ensure_ascii=False) + "\n" for i, f in enumerate(facts))
    r = subprocess.run(["go", "run", "./scripts/claimnumbers"], cwd=ROOT, input=lines, capture_output=True, text=True,
                       check=True)
    for line in r.stdout.splitlines():
        v = json.loads(line)
        facts[int(v["id"])]["missing"] = v["missing"]


def main():
    tags = sys.argv[1:] or ["m1Hpdf", "m1Hbj", "m1C", "R0", "R0rep"]
    facts = load_facts(tags)
    run_go(facts)
    no_evidence = sum(1 for f in facts if not (f["evidence"] or f["passage"]))
    dropped = [f for f in facts if f["missing"]]
    by_round = defaultdict(lambda: [0, 0])
    by_case = Counter()
    for f in facts:
        by_round[f["round"]][0] += 1
        if f["missing"]:
            by_round[f["round"]][1] += 1
            by_case[f["case"]] += 1
    rnd = random.Random(20261021)
    sample = rnd.sample(dropped, min(SAMPLE, len(dropped)))

    out = ["# Prova offline del controllo dei numeri (A1.1)", "",
           "> Generato da `scripts/qa_numbers_offline.py %s`. Spec: `docs/SPEC_QA_PACCHETTO_ONESTA.md` §2." % " ".join(tags),
           "> Evidenza = `evidence` + `passage` salvati con il fatto. Il controllo è lo stesso codice Go dell'estrattore.", "",
           "**Fatti letti:** %d · **scartati:** %d (%.1f%%) · senza evidenza salvata: %d" % (
               len(facts), len(dropped), 100.0 * len(dropped) / max(1, len(facts)), no_evidence), "",
           "## Per giro", "", "| Giro | Fatti | Scartati | % |", "|---|---|---|---|"]
    for r, (n, d) in sorted(by_round.items()):
        out.append("| %s | %d | %d | %.1f |" % (r, n, d, 100.0 * d / max(1, n)))
    out += ["", "## Per caso (scartati, tutti i giri)", "", "| Caso | Scartati |", "|---|---|"]
    out += ["| %s | %d |" % (c, n) for c, n in by_case.most_common()]
    out += ["", "## Campione di %d scarti da leggere" % len(sample), "",
            "Per ciascuno: **giusto** (il numero non c'è davvero nella pagina) o **falso allarme** (c'è, scritto in un altro modo).",
            ""]
    for i, f in enumerate(sample, 1):
        ev = (f["evidence"] or "").replace("\n", " ")
        pa = (f["passage"] or "").replace("\n", " ")
        out += ["### %d. %s · %s · %s" % (i, f["round"], f["case"], f["fid"]), "",
                "- **Numeri mancanti:** %s" % ", ".join(f["missing"]),
                "- **Fatto:** %s" % f["claim"].replace("\n", " "),
                "- **Evidenza:** %s" % ev[:600],
                "- **Brano:** %s" % (pa[:900] + ("…" if len(pa) > 900 else "")),
                "- **Fonte:** %s" % f["url"],
                "- **Giudizio:** _giusto / falso allarme_", ""]
    path = os.path.join(OUT, "qa_numeri_offline.md")
    with open(path, "w") as fh:
        fh.write("\n".join(out) + "\n")
    with open(os.path.join(OUT, "qa_numeri_offline.json"), "w") as fh:
        json.dump({"tags": tags, "facts": facts, "sample": [facts.index(s) for s in sample]}, fh, ensure_ascii=False)
    print("facts %d, dropped %d (%.1f%%), no evidence %d -> %s" % (
        len(facts), len(dropped), 100.0 * len(dropped) / max(1, len(facts)), no_evidence, path))


if __name__ == "__main__":
    main()
