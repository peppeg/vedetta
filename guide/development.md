# Development

Vedetta is one Go program with the web page inside. No Node, no Python at run time, no
C compiler.

## Building

Go 1.26 or later.

```
cp .env.example .env     # then edit it
make run                 # builds bin/vedetta and starts it on port 3030
```

The container image: `docker build -t vedetta .`

## Testing

```
make test-offline        # the Go tests, with the network off (Linux)
go test ./...            # the same, network on
make eval-test           # the tests of the measuring scripts (Python, standard library only)
deploy/test/install.sh   # both packages, with Docker, down to an answer from a stand-in model
```

GitHub runs the first ones at every push, and the installation when something that is
installed changes.

## Where things are

| Folder | What |
|---|---|
| `main.go`, `config/` | start, options, `.env` and `config.local` |
| `internal/agent/` | the research, the checks, the notes (`deeploop.go` and the `loop*.go` files) |
| `internal/llm/` | the OpenAI-compatible client |
| `internal/search/` | the search services and the ordering of results |
| `internal/reader/` | the page readers, PDFs, the address filter |
| `internal/cache/` | the memory of searches and pages |
| `internal/server/` | the web server, access, settings |
| `internal/storage/` | the searches (SQLite) |
| `web/dist/` | the web page: one file, no build |
| `deploy/` | the two packages |
| `scripts/` | the tools we measure Vedetta with; not part of the program |

Three interfaces are there to be extended: `search.Provider`, `reader.Reader`,
`search.Reranker`.

## Principles

- The model does the research; Go checks what can be checked. Few rules in the prompts.
- No rule made for one question. A fix must be general, or it is an option.
- When something is missing or fails, the answer says so.
- Vedetta talks to services through an address: it does not include them.

Comments and tests sometimes name stages of our work ("map R6", "case 24") and notes that
are not in this repository: they are only a trace of where a choice comes from.
