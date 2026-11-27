# Services and settings

Almost everything is set from the interface: **Settings**, or the page of the first run.
What is set there is kept in `data/config.local` and wins over `.env`.
[`.env.example`](../.env.example) lists the options most people need; the
[experiments](experiments.md) have a page of their own.

## The model

Any OpenAI-compatible service that can use **tools**: Ollama, LM Studio, llama.cpp, vLLM,
Lemonade, or a cloud service with your key. **Settings → Models** asks for an address, a
key if needed and the name; *Browse* lists the models the service offers.

- A key can be written as `${NAME}`: it stays in `.env` and is never shown in the
  interface.
- A key belongs to its address: if the address moves to another host, the key does not
  follow.
- Vedetta asks the service how much the model can hold, and never sends more than that.
- Sampling: temperature 1.0 and top_p 0.95 unless you change them. Some models want
  something else (Qwen: `VEDETTA_DEEP_LOOP_TOP_K=20`); `VEDETTA_DEEP_LOOP_TEMPERATURE=model`
  leaves it to the server.
- Free cloud models often use your questions for training: the list says so when the
  service declares it.

## Search

| Service | Needs |
|---|---|
| SearXNG | its address; JSON on, limiter off (see the [install guide](../deploy/README.md)) |
| [degoog](https://github.com/degoog-org/degoog) | its address (`DEGOOG_URL`, port 4444 by default); "Search API key enforcement" off in its settings |
| Brave, Tavily, Serper, Exa | a key; they are paid, so they start switched off |

Chosen in the top bar, switched on in **Settings → Services**. If the chosen one does
not respond, Vedetta tries the other free ones; the paid ones only if you allow it.

## Reading pages

| Reader | What it is | Needs |
|---|---|---|
| built in | a plain request and Vedetta's own text extractor; PDFs too | nothing |
| Jina Reader | a service with its own browser, self-hosted | its address |
| browser | a headless browser you already run, through the Chrome DevTools Protocol | its address (`BROWSER_CDP_URL`) |
| Firecrawl, Exa | cloud readers, paid | a key |

Some sites refuse a plain request. Without a second reader Vedetta cannot read those
pages, and the answer rests only on the ones it could open. The combination we use is
the built-in reader followed by Jina.

### A browser you already have

```
READER_PROVIDERS=http,browser,jina
BROWSER_CDP_URL=http://127.0.0.1:9222
```

Each page gets a new browser context, closed right after. Addresses of the home network
are blocked, also when a page asks for them. Tried on 20 October 2026 with
`chromedp/headless-shell` 151 and Lightpanda 1.0; another version may work, and Vedetta
says at start that it has not been tried. Vedetta does not hide what it is, and some sites
refuse a headless browser: keep Jina after it.

## Memory of searches and pages

Vedetta keeps searches for a day and pages for three hours, so that a similar question
does not run them again. A page taken from the memory tells the model the day it was
read. **Settings** shows how much it uses and can empty it.

## Experimental

Options that work but do not clearly improve the answers, or that are still being tried.
They start off. All the experiments, on and off, are in [Experiments](experiments.md).
