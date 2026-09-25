// Package mcp serves corpus operations as MCP tools over stdio (newline-delimited
// JSON-RPC 2.0). Tool results are the same agent-facing text the CLI prints.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/nikkoxgonzales/code-corpus/internal/app"
	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(ctx context.Context, a *app.App, args map[string]any) (interface{ Text() string }, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

var tools = []*tool{
	{Name: "corpus_search",
		Description: "Search reference source code in the local corpus. Use for 'how does X do Y' questions (natural language, ranked by relevance), " +
			"identifiers (finds definitions), exact strings, or regex. Results are repo:path:start-end locations with excerpts; " +
			"read more with corpus_show. Scope with repo when you know the project.",
		InputSchema: obj(map[string]any{
			"query":     str("natural-language question, identifier, quoted exact string, or regex"),
			"repo":      strs("limit to these repos (names from corpus_list)"),
			"lang":      strs("limit to languages, e.g. go, python, typescript"),
			"path":      str("path filter: glob like 'internal/*' or a substring"),
			"mode":      map[string]any{"type": "string", "enum": []string{"auto", "concept", "symbol", "exact", "regex"}, "description": "default auto"},
			"limit":     num("max results (default 8-10)"),
			"deep":      boolean("judge twice as many candidates (slower, better recall)"),
			"no_rerank": boolean("skip the relevance model; keyword ranking only"),
		}, "query"),
		run: func(ctx context.Context, a *app.App, m map[string]any) (interface{ Text() string }, error) {
			return a.Search(ctx, app.SearchArgs{Query: s(m, "query"), Mode: s(m, "mode"), Repos: ss(m, "repo"),
				Langs: ss(m, "lang"), Path: s(m, "path"), Limit: n(m, "limit"), Deep: b(m, "deep"), NoRerank: b(m, "no_rerank")})
		}},
	{Name: "corpus_def",
		Description: "Find where a symbol (function, type, class, method) is defined in the corpus. Accepts Name, pkg.Name or Type::method.",
		InputSchema: obj(map[string]any{"symbol": str("symbol name"), "repo": strs("limit to these repos"), "limit": num("max results")}, "symbol"),
		run: func(ctx context.Context, a *app.App, m map[string]any) (interface{ Text() string }, error) {
			return a.Search(ctx, app.SearchArgs{Query: s(m, "symbol"), Mode: "symbol", Repos: ss(m, "repo"), Limit: n(m, "limit")})
		}},
	{Name: "corpus_show",
		Description: "Read lines of a file in the corpus. target is 'repo:path' or 'repo:path:start-end' exactly as printed in search results.",
		InputSchema: obj(map[string]any{"target": str("repo:path[:start[-end]]"), "context": num("extra lines around the range")}, "target"),
		run: func(ctx context.Context, a *app.App, m map[string]any) (interface{ Text() string }, error) {
			return a.Show(s(m, "target"), n(m, "context"))
		}},
	{Name: "corpus_tree",
		Description: "Directory layout of a repo with file counts. Use to orient in an unfamiliar codebase.",
		InputSchema: obj(map[string]any{"repo": str("repo name"), "path": str("subdirectory (optional)"), "depth": num("levels to expand (default 2)")}, "repo"),
		run: func(ctx context.Context, a *app.App, m map[string]any) (interface{ Text() string }, error) {
			return a.Tree(s(m, "repo"), s(m, "path"), n(m, "depth"))
		}},
	{Name: "corpus_list",
		Description: "List repos in the corpus with ref, commit, size and languages.",
		InputSchema: obj(map[string]any{}),
		run: func(ctx context.Context, a *app.App, m map[string]any) (interface{ Text() string }, error) {
			return a.List()
		}},
	{Name: "corpus_add",
		Description: "Onboard GitHub repos into the corpus (shallow clone + index). Sources: owner/repo, owner/repo@tag, or URLs. Idempotent. Large repos can take a minute.",
		InputSchema: obj(map[string]any{"sources": strs("repos to add")}, "sources"),
		run: func(ctx context.Context, a *app.App, m map[string]any) (interface{ Text() string }, error) {
			return a.Add(ss(m, "sources"), "")
		}},
	{Name: "corpus_update",
		Description: "Fetch the latest commits for repos (all when repos is empty) and re-index changed files.",
		InputSchema: obj(map[string]any{"repos": strs("repos to update; empty = all")}),
		run: func(ctx context.Context, a *app.App, m map[string]any) (interface{ Text() string }, error) {
			return a.Update(ss(m, "repos"))
		}},
	{Name: "corpus_status",
		Description: "Health check: tools, reranker configuration, index problems and the command that fixes each.",
		InputSchema: obj(map[string]any{}),
		run: func(ctx context.Context, a *app.App, m map[string]any) (interface{ Text() string }, error) {
			return a.Status()
		}},
}

func s(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func n(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case string:
		var i int
		fmt.Sscan(v, &i)
		return i
	}
	return 0
}

func b(m map[string]any, k string) bool {
	v, _ := m[k].(bool)
	return v
}

// ss accepts an array of strings or a single (comma-separated) string.
func ss(m map[string]any, k string) []string {
	switch v := m[k].(type) {
	case string:
		if v == "" {
			return nil
		}
		return strings.Split(v, ",")
	case []any:
		var out []string
		for _, x := range v {
			if t, ok := x.(string); ok && t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

// Serve handles requests sequentially until in is closed.
func Serve(a *app.App, in io.Reader, out io.Writer, guide string) error {
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	send := func(r rpcResp) {
		mu.Lock()
		defer mu.Unlock()
		r.JSONRPC = "2.0"
		enc.Encode(r)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			send(rpcResp{ID: json.RawMessage("null"), Error: &rpcErr{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(req.ID) == 0 { // notification
			continue
		}
		res, rerr := handle(a, req, guide)
		send(rpcResp{ID: req.ID, Result: res, Error: rerr})
	}
	return sc.Err()
}

func handle(a *app.App, req rpcReq, guide string) (any, *rpcErr) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "code-corpus", "version": app.Version},
			"instructions":    guide,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{Code: -32602, Message: "invalid params"}
		}
		for _, t := range tools {
			if t.Name == p.Name {
				if p.Arguments == nil {
					p.Arguments = map[string]any{}
				}
				r, err := t.run(context.Background(), a, p.Arguments)
				if err != nil {
					e := xerr.As(err)
					msg := "error: " + e.Msg
					if e.Hint != "" {
						msg += " (fix: " + e.Hint + ")"
					}
					return textResult(msg, true), nil
				}
				return textResult(r.Text(), false), nil
			}
		}
		return nil, &rpcErr{Code: -32602, Message: "unknown tool " + p.Name}
	}
	return nil, &rpcErr{Code: -32601, Message: "method not found: " + req.Method}
}

func textResult(t string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": t}}, "isError": isErr}
}
