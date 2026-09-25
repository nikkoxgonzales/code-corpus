// Command corpus onboards GitHub repos into references/, keeps them updated, and
// searches them. Output is designed for coding agents: compact text by default,
// --json for a stable schema, one-line errors with fixes, and "# next:" hints.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"

	codecorpus "github.com/nikkoxgonzales/code-corpus"
	"github.com/nikkoxgonzales/code-corpus/internal/app"
	"github.com/nikkoxgonzales/code-corpus/internal/mcp"
	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

type result interface{ Text() string }

type exitCoder interface{ ExitCode() int }

type call struct {
	pos   []string
	flags map[string][]string
	ctx   context.Context
}

func (c *call) has(k string) bool { _, ok := c.flags[k]; return ok }

func (c *call) str(k string) string {
	if v := c.flags[k]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (c *call) list(k string) []string { return c.flags[k] }

func (c *call) int(k string) (int, error) {
	s := c.str(k)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, xerr.New(xerr.User, "--"+k+" takes a non-negative number", "bad value %q for --%s", s, k)
	}
	return n, nil
}

type command struct {
	name    string
	usage   string
	summary string
	bools   []string
	values  []string
	run     func(a *app.App, c *call) (result, error)
}

var aliases = map[string]string{
	"r": "repo", "l": "lang", "p": "path", "n": "limit", "m": "mode", "d": "depth", "c": "context", "j": "json",
}

var global = []string{"json"}
var globalValues = []string{"root"}

var commands []*command

func init() {
	commands = []*command{
		{name: "add", usage: "corpus add <owner/repo|url>[@ref] ... [--name N]",
			summary: "clone repos into references/ and index them (idempotent)",
			values:  []string{"name"},
			run: func(a *app.App, c *call) (result, error) {
				return a.Add(c.pos, c.str("name"))
			}},
		{name: "update", usage: "corpus update [repo ...]",
			summary: "fetch latest commits (all repos by default) and re-index changed files",
			run: func(a *app.App, c *call) (result, error) {
				return a.Update(c.pos)
			}},
		{name: "remove", usage: "corpus remove <exact-name> ...",
			summary: "delete a repo's clone and index",
			run: func(a *app.App, c *call) (result, error) {
				return a.Remove(c.pos)
			}},
		{name: "list", usage: "corpus list", summary: "list onboarded repos",
			run: func(a *app.App, c *call) (result, error) { return a.List() }},
		{name: "status", usage: "corpus status", summary: "check tools, reranker and index health; prints fixes",
			run: func(a *app.App, c *call) (result, error) { return a.Status() }},
		{name: "reindex", usage: "corpus reindex [repo ...]", summary: "rebuild the index from the checkouts",
			run: func(a *app.App, c *call) (result, error) { return a.Reindex(c.pos) }},
		{name: "search", usage: `corpus search "<query>" [--repo R] [--lang L] [--path GLOB] [--mode auto|concept|symbol|exact|regex] [--limit N] [--deep] [--no-rerank] [--budget BYTES] [--lines N]`,
			summary: "search the corpus (see: corpus guide)",
			bools:   []string{"deep", "no-rerank"},
			values:  []string{"repo", "lang", "path", "mode", "limit", "budget", "lines"},
			run:     runSearch("")},
		{name: "def", usage: "corpus def <symbol> [--repo R] [--lang L] [--limit N]",
			summary: "find where a symbol is defined",
			values:  []string{"repo", "lang", "path", "limit", "lines"},
			run:     runSearch("symbol")},
		{name: "show", usage: "corpus show <repo>:<path>[:<start>[-<end>]] [--context N]",
			summary: "print file lines with line numbers",
			values:  []string{"context"},
			run: func(a *app.App, c *call) (result, error) {
				if len(c.pos) != 1 {
					return nil, xerr.New(xerr.User, "corpus show repo:path:10-40", "show takes one target")
				}
				n, err := c.int("context")
				if err != nil {
					return nil, err
				}
				return a.Show(c.pos[0], n)
			}},
		{name: "tree", usage: "corpus tree <repo> [dir] [--depth N]",
			summary: "directory overview with file counts",
			values:  []string{"depth"},
			run: func(a *app.App, c *call) (result, error) {
				if len(c.pos) == 0 || len(c.pos) > 2 {
					return nil, xerr.New(xerr.User, "corpus tree <repo> [dir]", "tree takes a repo and optional dir")
				}
				d, err := c.int("depth")
				if err != nil {
					return nil, err
				}
				sub := ""
				repo := c.pos[0]
				if len(c.pos) == 2 {
					sub = c.pos[1]
				} else if r, s, ok := strings.Cut(strings.ReplaceAll(repo, ":", "/"), "/"); ok {
					repo, sub = r, s
				}
				return a.Tree(repo, sub, d)
			}},
		{name: "eval", usage: "corpus eval <cases.jsonl> [--mode M]",
			summary: "measure retrieval quality (hit@1, hit@5, MRR) with and without reranking",
			values:  []string{"mode", "limit"},
			run:     runEval},
	}
}

func runSearch(forced string) func(a *app.App, c *call) (result, error) {
	return func(a *app.App, c *call) (result, error) {
		if len(c.pos) == 0 {
			return nil, xerr.New(xerr.User, `corpus search "how does X handle Y"`, "missing query")
		}
		var nums [3]int
		for i, k := range []string{"limit", "budget", "lines"} {
			n, err := c.int(k)
			if err != nil {
				return nil, err
			}
			nums[i] = n
		}
		mode := c.str("mode")
		if forced != "" {
			mode = forced
		}
		return a.Search(c.ctx, app.SearchArgs{
			Query: strings.Join(c.pos, " "), Mode: mode, Repos: c.list("repo"), Langs: c.list("lang"),
			Path: c.str("path"), Limit: nums[0], Budget: nums[1], Lines: nums[2],
			NoRerank: c.has("no-rerank"), Deep: c.has("deep"),
		})
	}
}

func find(name string) *command {
	switch name {
	case "s", "find", "grep":
		name = "search"
	case "rm":
		name = "remove"
	case "ls":
		name = "list"
	case "pull", "sync":
		name = "update"
	case "cat", "read":
		name = "show"
	}
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

func parse(args []string, cmd *command) (*call, error) {
	c := &call{flags: map[string][]string{}}
	isBool := map[string]bool{}
	isVal := map[string]bool{}
	for _, b := range append(append([]string{}, global...), cmd.bools...) {
		isBool[b] = true
	}
	for _, v := range append(append([]string{}, globalValues...), cmd.values...) {
		isVal[v] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			c.pos = append(c.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" || isNumber(a) {
			c.pos = append(c.pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		if full, ok := aliases[name]; ok {
			name = full
		}
		switch {
		case isBool[name]:
			c.flags[name] = append(c.flags[name], "true")
		case isVal[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, xerr.New(xerr.User, cmd.usage, "--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			c.flags[name] = append(c.flags[name], val)
		default:
			return nil, xerr.New(xerr.User, cmd.usage, "unknown flag %s for %s", a, cmd.name)
		}
	}
	return c, nil
}

func isNumber(s string) bool { _, err := strconv.Atoi(s); return err == nil }

func usage() string {
	var b strings.Builder
	b.WriteString("corpus " + app.Version + ": reference source code for agents. Commands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-8s %s\n", c.name, c.summary)
	}
	b.WriteString("  guide    print the agent guide (start here)\n")
	b.WriteString("  mcp      run as an MCP server on stdio\n")
	b.WriteString("Global: --json (stable JSON output), --root DIR (default: $CORPUS_ROOT).\n")
	b.WriteString("# next: corpus guide\n")
	return b.String()
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if len(args) > 1 {
			if c := find(args[1]); c != nil {
				fmt.Printf("usage: %s\n%s\n", c.usage, c.summary)
				return 0
			}
		}
		fmt.Print(usage())
		return 0
	}
	switch args[0] {
	case "guide":
		fmt.Print(codecorpus.Guide)
		return 0
	case "version", "--version":
		fmt.Println("corpus " + app.Version)
		return 0
	}
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}

	if args[0] == "mcp" {
		c, err := parse(args[1:], &command{name: "mcp", usage: "corpus mcp [--root DIR]"})
		if err != nil {
			return fail(err, false)
		}
		a, err := openApp(c.str("root"))
		if err != nil {
			return fail(err, false)
		}
		defer a.Close()
		if err := mcp.Serve(a, os.Stdin, os.Stdout, codecorpus.Guide); err != nil {
			fmt.Fprintln(os.Stderr, "mcp:", err)
			return 1
		}
		return 0
	}

	cmd := find(args[0])
	if cmd == nil {
		names := []string{}
		for _, c := range commands {
			names = append(names, c.name)
		}
		sort.Strings(names)
		return fail(xerr.New(xerr.User, "commands: "+strings.Join(names, ", ")+", guide, mcp", "unknown command %q", args[0]), jsonOut)
	}
	c, err := parse(args[1:], cmd)
	if err != nil {
		return fail(err, jsonOut)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c.ctx = ctx
	a, err := openApp(c.str("root"))
	if err != nil {
		return fail(err, jsonOut)
	}
	defer a.Close()
	res, err := cmd.run(a, c)
	if err != nil {
		return fail(err, jsonOut)
	}
	if jsonOut {
		writeJSON(res)
	} else {
		fmt.Print(res.Text())
	}
	if ec, ok := res.(exitCoder); ok {
		return ec.ExitCode()
	}
	return 0
}

// writeJSON prints one compact JSON line without HTML escaping (<, >, & stay readable).
func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func openApp(root string) (*app.App, error) {
	r, err := app.FindRoot(root)
	if err != nil {
		return nil, err
	}
	return app.Open(r)
}

func fail(err error, jsonOut bool) int {
	e := xerr.As(err)
	if jsonOut {
		writeJSON(map[string]any{"error": e})
		return e.Code
	}
	msg := "error: " + e.Msg
	if e.Hint != "" {
		msg += " (fix: " + e.Hint + ")"
	}
	fmt.Fprintln(os.Stderr, msg)
	return e.Code
}
