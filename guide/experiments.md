# Experiments

A lot of things were tried while making Vedetta. What is on is what seemed to work best in
our tests; the rest stayed in the program, turned off. With another model, other search
engines or other questions you may find something else works better for you, so here is
the list: what each one does, what we saw, how to change it.

Most of our tests were made with one model, Qwen 3.8 Flash Next, on one machine at home.
"No difference" means none we could tell from chance: the same question, asked twice, never
gives the same answer.

## How to change one

- **From the interface:** the first three, and Deep Thought, are in **Settings → Experimental**. They apply
  from the next search.
- **From `.env`:** all the others, and the first three too. Add the line, then restart Vedetta (with the packages,
  `docker compose up -d`). For the first three, what is set in the interface wins.

Change one thing at a time, and ask the same question a few times before and after.

## Off

| What it does | What we saw | Line in `.env` |
|---|---|---|
| **Keep searching.** After the first draft, asks the model once whether there is more to look for | More pages read, often the original documents; about twice the time. Our model never answered "no, that is enough". The answers were not clearly better | `VEDETTA_DEEP_LOOP_PUSH=true` |
| **Special permission.** When a research reaches its limit but is still finding new pages, grants 40 more searches and readings, at most twice | With our model it almost never happens, so we do not know. It can matter with a model that reads a lot | `VEDETTA_DEEP_LOOP_GRANT=true` |
| **Rewrite only the flagged sentences.** After the checks the model rewrites those, not the whole answer | Faster, and cheaper on paid models. The other sentences are not looked at again | `VEDETTA_DEEP_LOOP_REVIEW_LINES=true` |
| **Compass.** Tells the model, before it starts, that what it remembers may be out of date | The model searched a bit more, but did not read more pages. No gain | `VEDETTA_DEEP_LOOP_COMPASS=true` |
| **Names as whole words.** A stricter check of names | Finds a few more invented names, and flags many that are only translated or written differently | `VEDETTA_DEEP_LOOP_NAME_WORDS=true` |
| **No language imposed on a search.** When the model does not name one, Vedetta searches in the language of the answer; with this, the search engine decides | No difference in general. It may help questions about foreign documents | `VEDETTA_DEEP_LOOP_SEARCH_LANG=auto` |
| **Second version of the built-in reader.** Keeps links, dates and empty cells of tables | Longer pages, no better answers | `VEDETTA_HTTP_READER=2` |
| **Italian preset.** Favours some Italian official sources, mostly on energy | Useful only for those questions | `VEDETTA_PRESET_IT=true` |
| **Embeddings** to order the search results, from a service of yours | Not measured in the present version | `EMBEDDINGS_URL=`… |
| **Deep Thought.** Answers any question, in seven and a half seconds and without searching anything | Always the same answer, and it is right. We are still looking for the question | only in Settings |

## On

| What it does | If you turn it off | Line in `.env` |
|---|---|---|
| **Ordering of the results** | The results stay in the order of the search engine. No difference in our tests: try it if your search engine is a good one | `VEDETTA_DEEP_LOOP_RERANK=false` |
| **Search again without quotes** when a search in quotes finds nothing | Fewer searches, some of them empty | `VEDETTA_DEEP_LOOP_QUOTE_FALLBACK=false` |
| **The model gets its own reasoning back** at every step | Less text to send | `VEDETTA_DEEP_LOOP_KEEP_REASONING=false` |
| **Follow-ups see the pages already read** | Every question starts from zero | `VEDETTA_DEEP_LOOP_SESSION_PAGES=false` |

The checks can be turned off too, one by one. They are the reason Vedetta exists, so this is
mainly to see what they change:

| Check | Line in `.env` |
|---|---|
| Numbers and names that are on no page go back to the model | `VEDETTA_DEEP_LOOP_REVIEW=false` |
| Pages seen only in a search preview are opened | `VEDETTA_DEEP_LOOP_REVIEW_PREVIEWS=false` |
| Cited pages the model did not read are opened and compared | `VEDETTA_DEEP_LOOP_LINK_CHECK=false` |
| A result the model did not open is not called a source | `VEDETTA_DEEP_LOOP_CITE_READ=false` |
| The passage that says each sentence | `VEDETTA_DEEP_LOOP_QUOTES=false` |

## Numbers you can change

| | Default | Line in `.env` |
|---|---|---|
| Searches and readings in a research | 40 | `VEDETTA_DEEP_LOOP_MAX_CALLS=40` |
| Results of each search | 10 | `SEARXNG_MAX_RESULTS=10` |
| Effort asked to the model (`low`, `medium`, `high`) | medium | `LLM_EFFORT_DEEP=medium` |
| Temperature (`model` leaves it to the server) | 1.0 | `VEDETTA_DEEP_LOOP_TEMPERATURE=1.0` |
| top_p, top_k | 0.95, off | `VEDETTA_DEEP_LOOP_TOP_P=0.95`, `VEDETTA_DEEP_LOOP_TOP_K=20` |

Once, with a model that reads a lot, we raised the limit from 40 to 120: it read many more
pages and used them, and took three minutes longer. It was a single question. With a paid
model, or a paid search, it costs more.

If you find a combination that works clearly better for you, tell us in an issue, with the
model you use: it is the kind of thing we cannot find out alone.
