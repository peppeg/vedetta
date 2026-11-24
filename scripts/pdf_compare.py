#!/usr/bin/env python3
"""Offline comparison of PDF text extractors on the PDF bench (V25-PDF.1).

For every PDF in data/pdf-bench/manifest.json (written by pdf_bench.py) and every
engine (pdfium and ledongthuc through scripts/pdfcmp, pdftotext as quality
reference), extract the text and measure time, peak memory and how many of the
verbatim fact quotes of the M1 rounds are found in it. The text of the round's
own reader (Jina/Firecrawl) is scored the same way as a baseline.

Quote matching works on normalized text (lowercase, unified quotes and dashes,
no markdown marks, words joined across line-break hyphens, single spaces):
  exact  = the whole normalized quote is in the text
  cover  = share of the quote's 4-word sequences found in the text

Usage:
  python3 scripts/pdf_compare.py [--bench data/pdf-bench] [--pdfcmp PATH]
"""
import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
import time
import unicodedata

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
ENGINES = ["pdfium", "ledongthuc", "pdftotext"]


def normalize(text):
    t = unicodedata.normalize("NFKC", text).lower()
    t = t.translate(str.maketrans({"’": "'", "‘": "'", "“": '"', "”": '"',
                                   "–": "-", "—": "-", "­": "",
                                   "\x02": "-"}))  # PDFium marks line-break hyphens with U+0002
    t = re.sub(r"(\w)-\s*\n\s*(\w)", r"\1\2", t)      # hyphenated line breaks
    t = re.sub(r"[*_#|>`\[\]]", " ", t)                # markdown marks
    t = re.sub(r"\s+", " ", t)
    return t.strip()


def ngrams(words, n=4):
    return {" ".join(words[i:i + n]) for i in range(len(words) - n + 1)} if len(words) >= n else {" ".join(words)}


def score(text, quotes):
    norm = normalize(text)
    grams = ngrams(norm.split())
    exact, cover = 0, 0.0
    for q in quotes:
        nq = normalize(q)
        if nq and nq in norm:
            exact += 1
            cover += 1.0
        else:
            qg = ngrams(nq.split())
            cover += len(qg & grams) / len(qg) if qg else 0.0
    return exact, cover


def run_engine(engine, pdf, out, pdfcmp):
    if engine == "pdftotext":
        cmd = ["pdftotext", "-enc", "UTF-8", pdf, out]
    else:
        cmd = [pdfcmp, "-engine", engine, pdf, out]
    start = time.monotonic()
    proc = subprocess.run(["/usr/bin/time", "-f", "RSS_KB=%M", *cmd], capture_output=True, text=True, timeout=600)
    wall = time.monotonic() - start
    rss = re.search(r"RSS_KB=(\d+)", proc.stderr)
    info = {"wall_s": round(wall, 2), "rss_mb": round(int(rss[1]) / 1024) if rss else None}
    if engine != "pdftotext":
        line = proc.stdout.strip().splitlines()[-1] if proc.stdout.strip() else "{}"
        info.update(json.loads(line))
    elif proc.returncode != 0:
        info["error"] = proc.stderr.strip()[:200]
    return info


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bench", default=os.path.join(REPO, "data", "pdf-bench"))
    ap.add_argument("--pdfcmp", help="pdfcmp binary (default: build scripts/pdfcmp into a temp dir)")
    args = ap.parse_args()

    tmp = tempfile.mkdtemp(prefix="pdfcmp-")
    pdfcmp = args.pdfcmp
    if not pdfcmp:
        pdfcmp = os.path.join(tmp, "pdfcmp")
        subprocess.run(["go", "build", "-o", pdfcmp, "."], cwd=os.path.join(HERE, "pdfcmp"), check=True)
    with open(os.path.join(args.bench, "manifest.json")) as f:
        manifest = json.load(f)

    rows = []
    totals = {e: {"exact": 0, "cover": 0.0, "pdfs_ok": 0} for e in ENGINES + ["reference"]}
    nquotes = 0
    for p in manifest["pdfs"]:
        pdf = os.path.join(args.bench, f"{p['id']}.pdf")
        row = {"id": p["id"], "pages": p["pages"], "kb": p["bytes"] >> 10, "quotes": len(p["quotes"]),
               "readers": p["readers"], "engines": {}}
        nquotes += len(p["quotes"])
        for e in ENGINES:
            out = os.path.join(args.bench, f"{p['id']}.{e}.txt")
            info = run_engine(e, pdf, out, pdfcmp)
            text = open(out, errors="replace").read() if os.path.exists(out) else ""
            info["chars"] = len(text)
            info["exact"], info["cover"] = score(text, p["quotes"])
            row["engines"][e] = info
            totals[e]["exact"] += info["exact"]
            totals[e]["cover"] += info["cover"]
            totals[e]["pdfs_ok"] += 1 if len(text.strip()) >= 250 and not info.get("error") else 0
        if p.get("reference"):
            text = open(os.path.join(args.bench, p["reference"]), errors="replace").read()
            ex, cov = score(text, p["quotes"])
            row["engines"]["reference"] = {"chars": len(text), "exact": ex, "cover": cov}
            totals["reference"]["exact"] += ex
            totals["reference"]["cover"] += cov
            totals["reference"]["pdfs_ok"] += 1
        rows.append(row)
        cells = "  ".join(
            f"{e}: {row['engines'][e].get('chars', 0) // 1000}k {row['engines'][e].get('exact', 0)}/{row['quotes']}"
            f" {row['engines'][e].get('wall_s', '-')}s {row['engines'][e].get('rss_mb', '-')}MB"
            + (" ERR" if row["engines"][e].get("error") else "")
            for e in ENGINES)
        print(f"{p['id']} {p['pages']:>4}p {row['kb']:>6}KB q{row['quotes']:<2} {cells}", flush=True)

    print(f"\nquotes total: {nquotes}")
    for e, t in totals.items():
        print(f"  {e:<11} exact {t['exact']:>3}/{nquotes}  cover {t['cover']:.1f}/{nquotes}  readable PDFs {t['pdfs_ok']}")
    with open(os.path.join(args.bench, "compare.json"), "w") as f:
        json.dump({"rows": rows, "totals": totals, "quotes": nquotes}, f, indent=2)


if __name__ == "__main__":
    sys.exit(main())
