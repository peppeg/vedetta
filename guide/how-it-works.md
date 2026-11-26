# How a research works

## The steps

**1. Research.** The model has two tools: `search` and `read`. It decides what to look
for, in which language, and which pages to open. A page is given to it whole, as far as
the model's memory allows. A research has room for 40 searches and readings; when they
are over, the model writes with what it has and says what it could not verify.

**2. Draft.** The model writes an answer with links to its sources. You do not see it yet.

**3. Checks.** Vedetta, not the model, looks in the draft for what does not stand:

| Check | What Vedetta does |
|---|---|
| Numbers and names | looks for them in everything the model saw. The ones it cannot find go back to the model: verify or remove. A name a page writes almost the same goes back with the page's spelling |
| Details seen only in a search preview | opens the page and sends the sentence back with the passage closest to it |
| Cited pages the model did not read | opens them and has each compared with the sentence, apart from the conversation. A sentence that does not stand goes back to the model; if it still does not, it loses the link |
| What the answer says it did | "I consulted the archives" is compared with the pages really opened |
| Addresses that came from no search and no page | opens them: if the page is not there, or does not name what the link says, the link goes |

The model corrects the draft, and may search and read a little more to do it.

**4. Answer.** You see it now, with the notes: searches made, pages read, sentences to
check. For each sentence Vedetta also asks the model for the passage that says it, and
looks for that passage on the page. Nothing in the answer changes at this point: it only
tells you where each sentence comes from.

**5. Follow-up questions.** The model sees the conversation and can use again the pages
it has already read.

## The marks

Marks appear only where there is something to say, so that the answer stays easy to read.

| Mark | Meaning |
|---|---|
| no mark | the model read the page |
| not read | the model did not read this page: at most it saw a preview |
| checked | the model did not read it; Vedetta opened it, and the sentence is there |
| in part | as above, but the sentence is there only in part |
| address | a project's address, not a source of the sentence |
| sentence to check | a number of the sentence is in no page read |

The line **Provenance** at the end says how the answer was made: researched, partial
(stopped before the end), from the model's memory (no source), or with problems along
the way.

## What the checks do not do

- They do not check judgments, advice and comparisons: those are the model's.
- They do not say whether a page is right, only whether the answer says what the page says.
- They do not check every sentence. The pages the model read are trusted to have been
  read; the notes say where each sentence comes from.
- A number found on a page may still be attached to the wrong thing.

## What changes the result

In our tests the model is what matters most: some read many pages, some stop at the
first ones. After the model come the search, which decides what can be found, and the
reader, which decides what can be opened. The method is the same for all.

Results vary between two runs of the same question: search engines return different
pages from day to day, and models do not write the same text twice.
