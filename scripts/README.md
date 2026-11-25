# scripts

The tools we measure Vedetta with. They are not part of the program: Vedetta runs without
this folder. They need Python 3 (standard library only) and, for some, Go.

They were written for our own work and machines. Some of their comments name notes that are
not in this repository. What we found with them is in [Measurements](../guide/measurements.md).

## What you can run

| Command | What it does | Needs |
|---|---|---|
| `make test-offline` | the Go tests, network off | Go, Linux |
| `make eval-test` | the tests of these scripts | Python |
| `make smoke` | three short questions on a running Vedetta | a running Vedetta |
| `make eval CASE=valencia-family-4d` | one question of `benchmark.json`, then its checks | a running Vedetta |
| `make judge SESSION=<id>` | asks another model whether each cited sentence is supported | a running Vedetta, a judge |
| `python3 scripts/bench.py --tag NAME --mode deep --live --no-judge` | a full round on a separate instance | see below |

## A round

`bench.py` builds Vedetta from the working tree into `~/vedetta-bench`, starts it on port
3031 with its own database and memory, asks the questions one after the other, saves the
answers and stops it. A Vedetta running on port 3030 is not touched.

- The settings are those of your `data/config.local` and `.env`. `--profile NAME` puts
  `profiles/NAME.json` on top; `${NAME}` in a profile is filled from `.env`, so no key is
  written there.
- `--live` searches the web and records it; without it, a round is answered from a saved
  corpus, which you have to make first.
- `--no-judge` spends nothing but your model. It ends with an estimate of what the judge
  would read; `--measure-only` runs the judge later on the saved round.
- While a round runs, the model should do nothing else, and no other model should be loaded.

The questions are in `benchmark.json`. Those called `known-…` are real questions with an
answer we had checked by hand. Their texts are not published, so the suite `known` runs only
here; the other cases can be run by anyone.

## The judge

The judge is another model, at any OpenAI-compatible address. Put it in `scripts/judge.env`
(not in git), or in variables with the same names:

```
JUDGE_BASE_URL=https://…/v1
JUDGE_MODEL=…
JUDGE_API_KEY=…
```

It costs: look at the estimate first. Two measures can be compared only with the same judge,
and a new judge has to be checked by hand on a few dozen sentences before it is believed.

## How we decide

- Two rounds of each version, on the same questions.
- The percentage is taken over all the sentences together, not case by case: a single case
  moves by a dozen points with nothing changed.
- A difference under 4 points, or with the two rounds on opposite sides, is no difference.

## The files

| File | What |
|---|---|
| `bench.py`, `bench_instance.sh` | rounds on the separate instance |
| `eval.py`, `benchmark.json`, `mixed_questions.json` | the questions and the deterministic checks |
| `free_report.py` | what can be counted without a judge: searches, pages, links, passages |
| `judge_pages.py` | each sentence against the pages that were read |
| `judge_links.py` | each sentence against the page it links |
| `judge_quotes.py`, `judge_review.py`, `judge_partial.py`, `judge.py` | passages, corrections, kinds of partial support, the earlier pipeline |
| `score_known.py` | the answers we already knew |
| `ruler.py`, `expected.json` | what an answer should have contained |
| `bench_report.py`, `compare_runs.py`, `bench_same_draft.py` | comparing rounds |
| `llm_fault_proxy.py`, `llm_replay_proxy.py` | a model that fails, a model that repeats itself |
| `pdf_*.py`, `reader_*.py`, `qa_numbers_offline.py` | offline comparisons of readers and checks |
| `browsercmp/`, `pdfcmp/` | candidate libraries, each a module of its own |
| `claimnumbers/`, `htmltext/`, `quoteprobe/`, `rerankreplay/` | the program's own code on saved texts |
| `profiles/` | settings for `--profile` |
| `test_*.py` | the tests of the above |
