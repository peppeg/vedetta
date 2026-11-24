#!/usr/bin/env python3
"""Collect the HTML pages that needed the fallback reader into a local bench (R7.1).

For the given round tags (scripts/out/server_TAG.log, runs_TAG.json):
  1. the non-PDF URLs served by a reader other than http/europepmc/pdf, plus the
     non-PDF URLs whose read failed in every reader (not DNS errors);
  2. a few control pages read fine by http (one per domain, at most --controls);
  3. the verbatim quotes of the facts taken from each page (benchmark instance DB);
  4. the text the round's reader extracted (live caches of the rounds), as reference.

Output in data/reader-bench/ (not versioned): NNN.ref-READER.txt, manifest.json, urls.txt

Usage:
  python3 scripts/reader_bench.py TAG [TAG ...] [--bench-dir ~/vedetta-bench] [--controls 10]

See docs/SPEC_R7_LETTORE_DI_RISERVA.md.
"""
import argparse
import json
import os
import re
from urllib.parse import urlparse

from pdf_bench import OUT, REPO, cached_texts, fact_quotes

SERVED_RE = re.compile(r"\[reader\] session=\S+ url=(\S+) served_by=(\S+)")
FAILED_RE = re.compile(r"\[read\] session=\S+ url=(\S+) status=failed (.*)")
PDFISH_RE = re.compile(r"(\.pdf|-pdf)([?#]|$)", re.I)
BASE_READERS = {"http", "europepmc", "pdf"}


def collect(tags):
    """url -> sorted readers ("failed" when every reader failed); control url -> ["http"]."""
    found, controls = {}, {}
    for tag in tags:
        with open(os.path.join(OUT, f"server_{tag}.log"), errors="replace") as f:
            text = f.read()
        for m in SERVED_RE.finditer(text):
            url, reader = m[1], m[2]
            if PDFISH_RE.search(url):
                continue
            if reader not in BASE_READERS:
                found.setdefault(url, set()).add(reader)
            elif reader == "http":
                controls.setdefault(urlparse(url).hostname, url)
        for m in FAILED_RE.finditer(text):
            if not PDFISH_RE.search(m[1]) and "no such host" not in m[2]:
                found.setdefault(m[1], set()).add("failed")
    return {u: sorted(r) for u, r in found.items()}, controls


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("tags", nargs="+", help="round tags with server_TAG.log and runs_TAG.json in scripts/out")
    ap.add_argument("--bench-dir", default=os.path.expanduser("~/vedetta-bench"))
    ap.add_argument("--out", default=os.path.join(REPO, "data", "reader-bench"))
    ap.add_argument("--controls", type=int, default=10, help="pages read by http, as control")
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)
    urls, controls = collect(args.tags)
    quotes = fact_quotes(args.tags, os.path.join(args.bench_dir, "data", "vedetta.db"))
    texts = cached_texts(args.bench_dir)
    fallback_hosts = {urlparse(u).hostname for u in urls}
    picked = [u for h, u in sorted(controls.items()) if h not in fallback_hosts and quotes.get(u)][:args.controls]

    manifest = []
    for url, readers in sorted(urls.items()) + [(u, ["http"]) for u in picked]:
        n = len(manifest) + 1
        ref = None
        if url in texts:
            ref = f"{n:03d}.ref-{readers[0]}.txt"
            with open(os.path.join(args.out, ref), "w") as f:
                f.write(texts[url])
        manifest.append({"id": f"{n:03d}", "url": url, "readers": readers, "control": readers == ["http"],
                         "quotes": quotes.get(url, []), "reference": ref})
    with open(os.path.join(args.out, "manifest.json"), "w") as f:
        json.dump({"tags": args.tags, "pages": manifest}, f, indent=2, ensure_ascii=False)
    with open(os.path.join(args.out, "urls.txt"), "w") as f:
        f.write("".join(p["url"] + "\n" for p in manifest))

    fb = [p for p in manifest if not p["control"]]
    by_reader = {}
    for p in fb:
        for r in p["readers"]:
            by_reader[r] = by_reader.get(r, 0) + 1
    print(f"{len(fb)} fallback pages ({', '.join(f'{k} {v}' for k, v in sorted(by_reader.items()))}), "
          f"{sum(len(p['quotes']) for p in fb)} quotes, {sum(1 for p in fb if p['reference'])} with reference text; "
          f"{len(manifest) - len(fb)} control pages, {sum(len(p['quotes']) for p in manifest if p['control'])} quotes")


if __name__ == "__main__":
    main()
