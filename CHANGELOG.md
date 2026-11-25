# Changelog

## 0.4.2 (25 November 2026)

The first public version. We have the answer; the question is up to you.

- **Research:** the model searches, opens pages, reads them whole and goes on as long as it
  needs, up to 40 searches and readings. You see each step and can stop it.
- **Checks before the answer:** numbers and names, details seen only in a preview, cited pages
  the model did not read, work the answer says it did, addresses that came from nowhere.
- **After the answer:** for each sentence, the passage of the page that says it, when there is one.
- **Follow-up questions**, with the pages already read.
- **Models:** any OpenAI-compatible service, at home or in the cloud.
- **Search:** SearXNG, Brave, Tavily, Serper, Exa. **Pages and PDFs:** built-in reader, Jina
  Reader, a browser you already have, Firecrawl, Exa.
- **Interface** in English and Italian, on the phone too; copy and print of an answer; the first
  run looks for the services you have.
- **Your data:** everything on your computer, an access code if you want one, a button to delete.
- **Two packages:** only Vedetta, or Vedetta with SearXNG and Jina Reader.

### Known problems

- A page that asks to log in can be counted as read.
- Vedetta may open a page to check one detail and not show the model the rest of it.
- "7 million and a half" is read as 7 million, and the number can be marked as not found.
- A number with more than 16 digits is compared, and shown, rounded.

What we measured, and what we did not, is in [Measurements](guide/measurements.md).
