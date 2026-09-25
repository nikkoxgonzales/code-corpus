# code-corpus — Plan

> **Status (2026-09-25): v0.1.0 implemented. Milestones 1–7 done.** Deviations from the original
> plan: Jev is reached through OpenRouter by default (TypeSafe direct also supported); reranked
> order blends p with the keyword score (weight 0.4, tuned by eval); queries get code-abbreviation
> and compound expansion; naming a repo in a query boosts it. Eval: hit@5 0.93, MRR 0.86 reranked
> vs 0.84 / 0.73 keyword-only (see README).

A local CLI + MCP server that lets coding agents **onboard** GitHub repos into `references/`,
**keep them updated**, and **search** them (one repo or all) with a keyword shortlist
followed by an optional Jev (TypeSafe AI "System One") relevance judgment.

Design rule for everything below: **the primary user is an agent.** Every command output,
error, and help text is written to be parsed and acted on by a model with a limited token budget.

---

## 1. Technical decisions

| Area | Decision | Why |
|---|---|---|
| Language | **Go** (1.27, installed) | Single static binary, fast, good concurrency for indexing and parallel Jev batches. Rust isn't installed. |
| Storage | **SQLite** via `modernc.org/sqlite` (pure Go, no cgo) | One file, FTS5 built in (BM25 + trigram tokenizer), transactional incremental updates. No cgo means it builds on Windows without gcc. |
| Exact / regex search | **ripgrep** (`rg --json`) | Already installed and the fastest option for literal and regex search. Runs directly on `references/`. |
| Concept search shortlist | **SQLite FTS5 BM25** over function-level chunks | This is where recall comes from; Jev only reorders what the shortlist finds. |
| Chunking | Built-in per-language **heuristic chunker** (regex definition detection + brace/indent block boundaries) for ~15 languages; fixed-size line windows as fallback | Tree-sitter in Go needs cgo. The chunker is behind an interface, so tree-sitter can replace it later. |
| Symbols | Extracted during chunking (name, kind, line) and stored in a `symbols` table | Makes "where is X defined" a single query. |
| Reranker | **Jev**, optional, behind a `Reranker` interface | The tool must work fully without an API key. Jev is 10 days old, so the interface keeps it swappable. |
| Git | Shell out to `git` (installed) | Simple and handles auth and proxies. Uses shallow, blobless clones. |
| Agent integration | CLI **and** MCP server (stdio) from the same binary | MCP lets agents call it natively; the CLI covers shells and scripts. |

Binary name: **`corpus`**.

---

## 2. Layout

```
F:\code-corpus\
  cmd/corpus/main.go
  internal/
    manifest/   corpus.json read/write
    gitops/     clone, fetch, diff of changed files
    index/      sqlite schema, incremental indexer
    chunk/      language detection + chunkers + symbol extraction
    search/     exact (rg), bm25, symbol, pipeline + ranking heuristics
    rerank/     Reranker interface, jev client, no-op
    output/     text + json renderers, error envelope
    mcp/        stdio MCP server exposing the same operations
  references/                  <- cloned repos: references/<owner>__<repo>/
  references/.corpus/          <- corpus.json manifest + index.db (git-ignored)
  AGENTS.md                    <- how an agent should use the tool (also printed by `corpus guide`)
  PLAN.md
```

---

## 3. Commands

All commands accept `--json`. The default output is **compact text** designed for agents
(see §5). Exit codes: `0` ok, `1` user error (with a hint), `2` not found, `3` network/git
failure, `4` partial success.

### Corpus management
| Command | Behavior |
|---|---|
| `corpus add <github-url\|owner/repo>[@ref] [--name N]` | Shallow clone (`--depth 1 --filter=blob:none --single-branch`), record it in the manifest, index it. Idempotent: re-adding an existing repo reports `already present` and exits 0. |
| `corpus update [name...\|--all]` | `git fetch --depth 1` + reset to the tracked ref, diff old..new SHAs, re-index only changed files. Reports `old_sha -> new_sha, N files changed` per repo. Repos run in parallel. |
| `corpus remove <name>` | Deletes the clone and its index rows. |
| `corpus list` | One line per repo: name, ref, short SHA, last updated, file/chunk count, primary languages. |
| `corpus status` | Detects drift (manifest vs. disk vs. index) and suggests fixes. |
| `corpus reindex [name]` | Full rebuild. |

### Search
| Command | Behavior |
|---|---|
| `corpus search "<query>" [--repo R]... [--lang L] [--path GLOB] [--limit N] [--mode auto\|exact\|regex\|concept\|symbol] [--no-rerank] [--deep]` | See §4. |
| `corpus def <symbol> [--repo R]` | Symbol definitions (shorthand for `--mode symbol`). |
| `corpus show <repo>:<path>[:<start>[-<end>]] [--context N]` | Prints exact file lines with line numbers. This is how an agent follows up on a hit. |
| `corpus tree <repo> [path] [--depth N]` | Compact directory overview, collapsing vendor, test and generated folders. |

### Agent onboarding
| Command | Behavior |
|---|---|
| `corpus guide` | Prints the contents of AGENTS.md: when to use each mode, examples, and token-cost tips. Kept under 1k tokens. |
| `corpus mcp` | Runs the stdio MCP server. Tools: `corpus_add`, `corpus_update`, `corpus_list`, `corpus_search`, `corpus_def`, `corpus_show`, `corpus_tree`. |

---

## 4. Search pipeline

```
query ─► mode detect ─► shortlist ─► heuristic score ─► [Jev judge] ─► threshold ─► pack excerpts ─► output
```

1. **Mode detection** (`auto`):
   - An identifier (`parseConfig`, `http.Client`) → `symbol`, then `exact`.
   - Regex metacharacters → `regex`.
   - Several natural-language words → `concept`.
2. **Shortlist:**
   - `exact`/`regex`: rg over the selected repos, mapping each hit to its enclosing chunk.
   - `symbol`: symbols table lookup, with exact matches ranked above prefix matches.
   - `concept`: FTS5 BM25 over chunks, with the query expanded by identifier splitting (`parseConfig` → `parse config`), stopwords dropped and OR'd terms. Chunk text is indexed together with its file path and symbol name, so path and name matches count. Collect about 60 candidates.
3. **Heuristic score** (always applied, no API): BM25 score, plus a boost for definitions, plus a boost when the query term is in the symbol or file name. Penalties for tests, vendor, generated files, examples and docs, unless `--path` targets them. Also cap the number of chunks per file so one file can't dominate.
4. **Jev judge** (concept mode only, when `TYPESAFE_API_KEY` is set and `--no-rerank` isn't passed):
   - The top 30 candidates go into one `system_one` call: state = the query, questions = one Noul per chunk ("Does this code implement or directly answer: <query>?"). Each chunk is trimmed to about 60 lines.
   - `--deep` sends a second batch of 30.
   - Keep chunks with p ≥ 0.5, order them by p, and use the heuristic score to break ties.
   - Timeout of 4s; on timeout or error, fall back to the heuristic order and flag `reranked: false` in the output.
5. **Pack excerpts:** merge adjacent or overlapping chunks from the same file and cap output at about 8 KB by default (`--budget`). Each hit shows the location, symbol, score and a trimmed excerpt with line numbers.

Estimated cost per concept search with Jev: about 30 × 400 tokens = 12k tokens ≈ **$0.0005**.

---

## 5. Agent-first output contract

Default text output (short and grep-like, with no ANSI colors, spinners or progress bars on stdout):

```
# 3 results for "how are retries backed off" (concept, reranked, 212ms)
[1] grpc-go:internal/backoff/backoff.go:41-78  func (bc Exponential) Backoff  p=0.94
    41 | func (bc Exponential) Backoff(retries int) time.Duration {
    ...
[2] ...
# next: corpus show grpc-go:internal/backoff/backoff.go:30-90
```

Rules:
- The **`repo:path:line` format is identical everywhere**, so any hit can be pasted straight into `show`.
- Every response ends with at most one `# next:` hint suggesting the most useful follow-up command.
- Errors go to stderr as one line plus a fix hint, e.g. `error: repo "grpc" not found. did you mean: grpc-go? (corpus list)`.
- Empty results are never a bare "no results". The output says what was searched and suggests a broader mode, e.g. `# 0 results in 4 repos (exact). try: corpus search "..." --mode concept`.
- `--json` uses a stable schema with `schema_version`. Every result has `repo`, `path`, `start_line`, `end_line`, `symbol`, `kind`, `score`, `relevance`, `excerpt`. The envelope has `query`, `mode`, `reranked`, `took_ms`, `truncated`, `next`.
- Deterministic ordering, so identical queries give identical output.
- Long operations (`add`, `update --all`) print one summary line per repo when each finishes, never interleaved partial lines.
- MCP tool descriptions are written as instructions ("Use for questions about how code works; use corpus_def when you know the name").

---

## 6. Index schema (SQLite)

```
repos(id, name UNIQUE, url, ref, sha, updated_at, file_count, chunk_count)
files(id, repo_id, path, lang, sha1, size, is_test, is_vendor, is_generated)
chunks(id, file_id, start_line, end_line, symbol, kind, text)
chunks_fts  USING fts5(text, symbol, path, content='chunks', tokenize='unicode61 ...')   -- BM25
symbols(id, repo_id, file_id, name, name_lower, kind, line)                               -- def lookup
```

- Files are skipped if they're binary, over 1 MB, lockfiles, minified or under `node_modules`/`vendor`/`dist`. Vendored and test code is flagged rather than dropped.
- Incremental: on update, only paths from `git diff --name-status` are deleted and re-inserted. The file's sha1 guards against unchanged content.

---

## 7. Milestones

1. **Skeleton:** Go module, manifest, `add`/`list`/`remove`/`update` with git, text and JSON output, exit codes, `guide`.
2. **Exact search:** rg integration, `show`, `tree`, repo/lang/path filters.
3. **Index:** chunker plus symbols, SQLite FTS5, incremental reindex, `def`, `concept` mode with heuristic ranking.
4. **Jev reranker:** client (verify the endpoint and request shape against the TypeSafe docs first), batching, timeout fallback, `--deep`, and a cost line in the output.
5. **MCP server:** the same operations exposed as tools. Register it in Claude Code settings.
6. **Evaluation:**
   - Onboard 5–10 real repos (e.g. Go, TS and Python projects).
   - Write about 40 question → expected-location pairs.
   - Measure hit@5 and MRR for three setups: heuristic only, Jev rerank, and plain rg as the baseline.
   - Tune the chunker, query expansion and thresholds against these numbers.
7. **Polish:** AGENTS.md, `status`, parallel update, a README with install instructions.

Done means an agent given only `corpus guide` can onboard a repo, answer "how does X do Y" in 1–2 tool calls, and update the corpus, all without reading raw file dumps.

---

## 8. Open risks
- **Recall is limited by the shortlist.** The eval in milestone 6 is how we catch that.
- **Jev API details** (endpoint, auth header, batch limits) come from secondary sources and need verifying at milestone 4.
- **Code snippets are sent to TypeSafe** when reranking. This is acceptable for public repos, and `--no-rerank` or `CORPUS_NO_RERANK=1` switches it off globally.
- **Heuristic chunking** will be imperfect for some languages. It's isolated behind an interface, so it can be upgraded to tree-sitter later.
