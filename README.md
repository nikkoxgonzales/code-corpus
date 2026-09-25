# code-corpus

A local reference-source corpus for coding agents. `corpus` shallow-clones GitHub
repos into `references/`, keeps them updated, and searches them with a keyword
shortlist that is optionally re-ranked by a System One relevance model (TypeSafe Jev,
through OpenRouter or TypeSafe directly).

Agent-facing usage lives in [AGENTS.md](AGENTS.md) (also printed by `corpus guide`
and sent as MCP server instructions).

## Build

```sh
go build -o corpus.exe ./cmd/corpus     # Go 1.27+, no cgo
```

Runtime needs `git`; `rg` (ripgrep) is used for exact/regex search when present
(falls back to `git grep`).

## Where the corpus lives

`references/` next to the binary, or set `CORPUS_ROOT=<dir containing references/>`,
or pass `--root`. Clones are `references/<name>/`; the manifest and SQLite index are in
`references/.corpus/`.

## Reranking (optional)

| env | default | meaning |
|---|---|---|
| `OPENROUTER_API_KEY` | | use OpenRouter (`typesafe/jev-1.13`); preferred when both keys exist |
| `TYPESAFE_API_KEY` | | use TypeSafe directly (`jev-latest`) |
| `CORPUS_RERANK` | `auto` | `off`, `auto`, `openrouter`, `typesafe` |
| `CORPUS_RERANK_MODEL` | per provider | override the model |
| `CORPUS_RERANK_BASE_URL` | per provider | override the endpoint base (`/v1/systemone` is appended) |
| `CORPUS_RERANK_TIMEOUT_MS` | `8000` | on timeout, results fall back to keyword order |

Only the query and up to 30 candidate excerpts (60 with `--deep`) are sent. Typical cost is
about $0.0005 per concept search. `--no-rerank` or `CORPUS_RERANK=off` keeps everything local.

## MCP

```sh
claude mcp add corpus -s user -- F:\code-corpus\corpus.exe mcp
```

Tools: `corpus_search`, `corpus_def`, `corpus_show`, `corpus_tree`, `corpus_list`,
`corpus_add`, `corpus_update`, `corpus_status`.

## Quality

`corpus eval eval/cases.jsonl` runs 44 questions over grpc-go, requests, express, ky
and ripgrep. At the time of writing:

| config | hit@1 | hit@5 | MRR | avg latency |
|---|---|---|---|---|
| keyword only | 0.66 | 0.84 | 0.73 | 12 ms |
| + Jev rerank | 0.80 | 0.93 | 0.86 | ~370 ms |

## Layout

```
cmd/corpus        CLI entry, arg parsing, eval
internal/app      operations shared by CLI and MCP (add/update/list/show/tree/status)
internal/search   pipeline: mode detection, ripgrep, BM25 + heuristics, rerank, excerpts
internal/index    SQLite schema, incremental indexer (by git blob), FTS5, symbols
internal/chunk    per-language definition chunker + symbol extraction (regex, no cgo)
internal/rerank   System One client (OpenRouter / TypeSafe)
internal/mcp      stdio JSON-RPC MCP server
internal/gitops   shallow clone / update / ls-files
```
