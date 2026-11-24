#!/usr/bin/env python3
"""Collect the PDFs read in benchmark rounds into a local bench (V25-PDF.0).

For the given round tags (scripts/out/server_TAG.log, runs_TAG.json):
  1. the URLs served by a reader other than http/europepmc (PDFs always go past http),
     plus the PDF-looking URLs whose read failed (reader "failed");
  2. download each one once; keep it only if it is a real PDF (starts with %PDF-);
  3. the verbatim quotes of the facts taken from it (benchmark instance DB);
  4. the text the reader of the round extracted (live caches of the rounds), as reference.

Output in data/pdf-bench/ (not versioned):
  NNN.pdf, NNN.ref-READER.txt, manifest.json

Usage:
  python3 scripts/pdf_bench.py m1C_1 m1C_2 m1H_1 m1H_2 [--bench-dir ~/vedetta-bench] [--out data/pdf-bench]

See docs/SPEC_PDF_LETTORE_INTERNO.md.
"""
import argparse
import glob
import json
import os
import re
import sqlite3
import subprocess
import sys
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
OUT = os.path.join(HERE, "out")
MAX_BYTES = 80 << 20
SERVED_RE = re.compile(r"\[reader\] session=\S+ url=(\S+) served_by=(\S+)")
FAILED_RE = re.compile(r"\[read\] session=\S+ url=(\S+) status=failed")
PDFISH_RE = re.compile(r"(\.pdf|-pdf)([?#]|$)", re.I)
SKIP_READERS = {"http", "europepmc"}
UA = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"


def served_urls(tags):
    """url -> sorted readers that served it in the rounds."""
    found = {}
    for tag in tags:
        with open(os.path.join(OUT, f"server_{tag}.log"), errors="replace") as f:
            text = f.read()
        for m in SERVED_RE.finditer(text):
            if m[2] not in SKIP_READERS:
                found.setdefault(m[1], set()).add(m[2])
        for m in FAILED_RE.finditer(text):
            if PDFISH_RE.search(m[1]):
                found.setdefault(m[1], set()).add("failed")
    return {u: sorted(r) for u, r in found.items()}


def fact_quotes(tags, db_path):
    """url -> verbatim snippets of the facts that cite it, over the rounds' sessions."""
    sessions = []
    for tag in tags:
        with open(os.path.join(OUT, f"runs_{tag}.json")) as f:
            sessions += json.load(f).values()
    quotes = {}
    db = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    for sid in sessions:
        for (facts_json,) in db.execute(
                "SELECT facts_json FROM messages WHERE session_id = ? AND facts_json != ''", (sid,)):
            for fact in json.loads(facts_json) or []:
                q = (fact.get("verbatim_snippet") or "").strip()
                if q and fact.get("source_url"):
                    quotes.setdefault(fact["source_url"], [])
                    if q not in quotes[fact["source_url"]]:
                        quotes[fact["source_url"]].append(q)
    return quotes


def cached_texts(bench_dir):
    """url -> extracted text, from the live caches of the rounds."""
    texts = {}
    for path in glob.glob(os.path.join(bench_dir, "data", "cache-live-*", "page", "**", "*"), recursive=True):
        if not os.path.isfile(path):
            continue
        try:
            with open(path) as f:
                doc = json.load(f).get("document") or {}
        except (OSError, ValueError):
            continue
        if doc.get("url") and doc.get("content"):
            texts[doc["url"]] = doc["content"]
    return texts


def download(url):
    req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/pdf,*/*"})
    with urllib.request.urlopen(req, timeout=90) as resp:
        data = resp.read(MAX_BYTES + 1)
    if len(data) > MAX_BYTES:
        raise ValueError(f"larger than {MAX_BYTES >> 20} MB")
    return data


def pdf_pages(path):
    try:
        out = subprocess.run(["pdfinfo", path], capture_output=True, text=True, timeout=30).stdout
    except (OSError, subprocess.TimeoutExpired):
        return None
    m = re.search(r"^Pages:\s+(\d+)", out, re.M)
    return int(m[1]) if m else None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("tags", nargs="+", help="round tags with server_TAG.log and runs_TAG.json in scripts/out")
    ap.add_argument("--bench-dir", default=os.path.expanduser("~/vedetta-bench"))
    ap.add_argument("--out", default=os.path.join(REPO, "data", "pdf-bench"))
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)
    urls = served_urls(args.tags)
    quotes = fact_quotes(args.tags, os.path.join(args.bench_dir, "data", "vedetta.db"))
    texts = cached_texts(args.bench_dir)
    manifest, skipped = [], []
    for url, readers in sorted(urls.items()):
        try:
            data = download(url)
        except Exception as e:  # noqa: BLE001 - report every failure and go on
            skipped.append({"url": url, "reason": f"download failed: {e}"})
            continue
        if not data.startswith(b"%PDF-"):
            skipped.append({"url": url, "reason": "not a PDF"})
            continue
        n = len(manifest) + 1
        pdf = os.path.join(args.out, f"{n:03d}.pdf")
        with open(pdf, "wb") as f:
            f.write(data)
        ref = None
        if url in texts:
            ref = os.path.join(args.out, f"{n:03d}.ref-{readers[0]}.txt")
            with open(ref, "w") as f:
                f.write(texts[url])
        manifest.append({"id": f"{n:03d}", "url": url, "readers": readers, "bytes": len(data),
                         "pages": pdf_pages(pdf), "quotes": quotes.get(url, []),
                         "reference": os.path.basename(ref) if ref else None})
        print(f"{n:03d} {len(data) >> 10:>6} KB  pages {manifest[-1]['pages']}  quotes {len(manifest[-1]['quotes'])}  {url}")
    with open(os.path.join(args.out, "manifest.json"), "w") as f:
        json.dump({"tags": args.tags, "pdfs": manifest, "skipped": skipped}, f, indent=2, ensure_ascii=False)
    pdf_skips = [s for s in skipped if s["reason"] != "not a PDF"]
    print(f"{len(manifest)} PDFs, {sum(len(m['quotes']) for m in manifest)} quotes; "
          f"{len(skipped) - len(pdf_skips)} non-PDF pages skipped, {len(pdf_skips)} downloads failed")
    for s in pdf_skips:
        print(f"  failed: {s['url']} ({s['reason']})", file=sys.stderr)


if __name__ == "__main__":
    main()
