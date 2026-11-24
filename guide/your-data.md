# Your data

Everything stays on the machine where Vedetta runs, in the folder `data/` (with Docker
or Podman, the volume `vedetta-data`). Nothing is sent to the project: no statistics, no
automatic reports.

The services you choose receive what they need to work: a cloud model gets the
conversation, a paid search gets the searches, a cloud reader gets the addresses. Their
own privacy rules apply.

## What is kept

| What | Where | What it holds | How to delete it |
|---|---|---|---|
| Searches | `data/vedetta.db` (with `-wal` and `-shm`) | questions, answers, pages read, notes | from the interface, one by one or all together (**Settings → Your data**). The deleted text is overwritten on disk |
| Memory of searches and pages | `data/cache/` | search results (a day) and the text of pages read (three hours) | **Settings → Empty now**; what expires is deleted by itself |
| Settings and keys | `data/config.local`, `.env` | addresses, keys, choices. Of the codes set from the interface, only a fingerprint | they are files; `config.local` is readable only by your user |
| PDF engine | `data/pdfium-cache/` | the program that reads PDFs, ready to run | nothing of yours in it; it is made again when needed |
| Log | the container's log, or the systemd journal | the steps of each research, the searches, the addresses read, the errors | see below |

## The log

The log helps to understand failures. It contains the searches the model writes, which
often repeat words of your question, and the addresses of the pages read.

- **Fewer details:** **Settings → Your data → Log with fewer details**, or
  `VEDETTA_QUIET_LOG=true`. The log keeps steps, counts and errors, without searches or
  addresses. It applies to new lines.
- **Deleting it, with Docker or Podman:** the log goes away with the container.

  ```
  docker compose up -d --force-recreate vedetta
  ```

- **Deleting it, with systemd** (user service). This deletes the whole journal of your
  user, not only Vedetta's lines:

  ```
  journalctl --user --rotate
  journalctl --user --vacuum-time=1s
  ```

## Deleting everything

With Docker or Podman, from the folder of the package you installed:

```
docker compose down -v
rm .env
```

Without containers, with Vedetta stopped:

```
rm -rf data/
rm .env
```
