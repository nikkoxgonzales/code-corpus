# corpus — reference source code for agents

`corpus` keeps clones of GitHub repos in `references/` and searches them. Use it
to learn how a library or project *actually* implements something instead of guessing.

## Workflow
1. `corpus list` — see what is onboarded.
2. `corpus add owner/repo` — onboard (shallow clone + index). Also: `owner/repo@v1.2.0`, full URLs, several at once.
3. `corpus search "<question or identifier>"` — find code. Add `--repo name` to scope.
4. `corpus show repo:path:start-end` — read exact lines. Every hit prints a location in this format.
5. `corpus update` — pull latest for all repos (only changed files are re-indexed).

## Search modes (`--mode`, default auto)
- `concept`  natural language: `"how are retries backed off"`. Keyword shortlist, then ranked
             by a relevance model when OPENROUTER_API_KEY/TYPESAFE_API_KEY is set (p=0.00-1.00 per hit).
- `symbol`   definitions by name: `parseConfig`, `http.Client`, `Foo::bar`. Same as `corpus def X`.
- `exact`    literal text (smart case). Quote the query in auto mode: `'"retry budget"'`.
- `regex`    ripgrep regex: `"func \w+Handler"`.
Auto picks: identifier -> symbol (falls back to exact), short regex -> regex, otherwise concept.

## Filters and size
`--repo a,b`  `--lang go`  `--path "internal/*"` (no wildcard = substring)  `--limit N`
`--deep` judges 60 candidates instead of 30. `--no-rerank` stays fully local.
Output is capped (~8 KB, `--budget`); later hits then show location only.

## Reading results
```
# 2 results for "retry backoff" | concept | reranked by openrouter:typesafe/jev-1.13 ($0.00041) | 3 repos | 640ms
[1] grpc-go:internal/backoff/backoff.go:41-78  method Backoff  p=0.94
 41| func (bc Exponential) Backoff(retries int) time.Duration {
# next: corpus show grpc-go:internal/backoff/backoff.go:21-118
```
- `p` = probability the hit answers the query (order also weighs keyword strength).
  `weak` = nothing passed 0.5: locations only; rephrase, add `--deep`, or scope with `--repo`.
- Naming a repo in the query ("how does ky retry") boosts that repo.
- `[test] [vendor] [generated] [doc]` tags; these are ranked lower unless the query asks for them.
- `>` marks matching lines in exact/regex/symbol results.
- `# next:` is the most useful follow-up command. Errors print `error ... (fix: ...)`.

## Other commands
`corpus tree repo [dir] [--depth N]` layout overview · `corpus def Name` · `corpus status` health + fixes ·
`corpus reindex [repo]` · `corpus remove <exact-name>` · `corpus mcp` MCP server (same tools).
Every command takes `--json` (stable schema, `schema_version`). Exit codes: 0 ok, 1 usage, 2 not found, 3 network, 4 partial.

## Tips
- Start broad (`concept`), then pin down with `symbol`/`exact` and `show`.
- Use identifiers from hits in follow-up queries; they rank very well.
- Scope with `--repo` when you know the project; searching everything is fine too.
