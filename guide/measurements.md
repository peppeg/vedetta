# Measurements

The last rounds before the first public version. They say how Vedetta did on our questions,
with our model, on our machine. They are not a score of how often it is right in general.

## What we did

- **When:** 25 November 2026, on commit `a5c16f0`.
- **With what:** Qwen 3.8 Flash Next on Halogen (Strix Halo), SearXNG, the built-in reader and
  then Jina Reader. The default settings, except two: `top_k` at 20, as that model is meant to
  be used, and the date a page was read not told to the model.
- **What we asked:** 19 questions, 16 in Italian and 3 in English, twice: 38 answers. They are our own: technical problems,
  law, news, medicine, history, a question with no answer. The texts of the ones taken from
  real use are not published.
- **Who judged:** another model, GLM-5.3-Flash, which did not know what had written the
  answers. We read by hand all the items it did not call right, and all the sentences it
  called not supported. We did not read again the ones it called right.

## What we found

| | First round | Second round | Together |
|---|---|---|---|
| Things the answer had to say, and said right | 73 of 80 | 71 of 80 | 144 of 160 |
| … wrong | 1 | 3 | 4 |
| … missing | 6 | 6 | 12 |
| Errors we were waiting for, made | 3 of 33 | 4 of 33 | 7 of 66 |
| Sentences supported by the pages read | 84.7% | 85.3% | 85.0% of 1,181 |
| … only in part | 78 | 65 | 143 |
| … not supported | 12 | 22 | 34 (2.9%) |
| Sentences with a link to a page the model read: all there | 96 of 145 | 103 of 151 | 199 of 296 (67%) |
| … in part | 48 | 47 | 95 |
| … not there | 1 | 1 | 2 |
| What an answer should contain (9 questions) | 34 of 37 | 34 of 37 | |
| Pages read | 87 | 80 | |
| Pages that could not be read | 9 | 20 | |
| Time for an answer, median | 3 min | 3 min 17 s | |
| Sentences with a link | 27% | 30% | |
| Sentences whose passage was found on a page | 84% | 70% | |

One of the questions had grown old: a new version of PostgreSQL had come out six days
before. The answers had it, our list did not. Without that question, 143 of 156.

**The sentences that are not supported** are mostly text the question asked for (a letter),
advice and judgments. Six are facts that no page read says: for example a dose attributed to
studies that do not give it, and a key of a keyboard.

**Other errors we could trace:** a password taken from the manual of another firmware; a
figure taken from a weak site; options of a program that had changed since.

**Other things that went wrong:** in 2 answers of 38 the model used up its searches. One
answer opened no page at all. In 2 the passages could not be looked for.

## How much it varies

The two rounds are the same program, on the same day, with the same questions. Look at the
last line of the table: 84% and 70%. Two weeks earlier, on the same questions, an earlier
version had 139 of 160 and 12 errors of 66. It looks like progress, but we cannot tell it
from chance.

## What we did not measure

- A cloud model with this version. It costs, and we had no more to spend.
- Other models with this version. In earlier rounds the model changed the result more than
  anything else.
- Conversations: here every question stands alone.
- Languages other than Italian and, for three questions, English.
- Whether the answers are useful. That is for whoever asked.

After these rounds one note under the answers was corrected: a passage too short to prove
anything is no longer called "not found". It does not change the answers.

The tools are in [scripts](../scripts/README.md).
