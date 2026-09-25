package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/nikkoxgonzales/code-corpus/internal/app"
	"github.com/nikkoxgonzales/code-corpus/internal/rerank"
	"github.com/nikkoxgonzales/code-corpus/internal/search"
	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

// An eval case: {"q": "...", "repo": "optional scope", "expect": ["repo:path[:line]", ...]}.
// A hit matches when repo and path match and, if a line is given, the hit covers it.
type evalCase struct {
	Q      string   `json:"q"`
	Repo   string   `json:"repo"`
	Mode   string   `json:"mode"`
	Expect []string `json:"expect"`
}

type evalConfig struct {
	Name  string  `json:"name"`
	Hit1  float64 `json:"hit_at_1"`
	Hit5  float64 `json:"hit_at_5"`
	MRR   float64 `json:"mrr"`
	AvgMs int64   `json:"avg_ms"`
	Cost  float64 `json:"cost_usd"`
}

type evalRow struct {
	Q     string         `json:"q"`
	Ranks map[string]int `json:"ranks"` // config -> 1-based rank, 0 = miss
}

type evalResult struct {
	Cases   int           `json:"cases"`
	Configs []*evalConfig `json:"configs"`
	Rows    []*evalRow    `json:"rows"`
}

func runEval(a *app.App, c *call) (result, error) {
	if len(c.pos) != 1 {
		return nil, xerr.New(xerr.User, "corpus eval cases.jsonl", "eval takes one file")
	}
	f, err := os.Open(c.pos[0])
	if err != nil {
		return nil, xerr.New(xerr.NotFound, "", "%v", err)
	}
	defer f.Close()
	var cases []evalCase
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var ec evalCase
		if err := json.Unmarshal([]byte(line), &ec); err != nil {
			return nil, xerr.New(xerr.User, `each line: {"q":"...","expect":["repo:path"]}`, "line %d: %v", n, err)
		}
		cases = append(cases, ec)
	}
	limit, _ := c.int("limit")
	if limit == 0 {
		limit = 10
	}
	configs := []*evalConfig{{Name: "keyword"}}
	if rc, _ := rerank.FromEnv(); rc != nil {
		configs = append(configs, &evalConfig{Name: "reranked"})
	}
	res := &evalResult{Cases: len(cases), Configs: configs}
	for _, ec := range cases {
		row := &evalRow{Q: ec.Q, Ranks: map[string]int{}}
		res.Rows = append(res.Rows, row)
		for _, cfg := range configs {
			mode := ec.Mode
			if m := c.str("mode"); m != "" {
				mode = m
			}
			var repos []string
			if ec.Repo != "" {
				repos = []string{ec.Repo}
			}
			r, err := a.Search(c.ctx, app.SearchArgs{Query: ec.Q, Mode: mode, Repos: repos, Limit: limit,
				NoRerank: cfg.Name == "keyword", Budget: 1})
			if err != nil {
				return nil, err
			}
			cfg.AvgMs += r.TookMs
			cfg.Cost += r.CostUSD
			rank := 0
			for i, h := range r.Hits {
				if matchesAny(h, ec.Expect) {
					rank = i + 1
					break
				}
			}
			row.Ranks[cfg.Name] = rank
			if rank == 1 {
				cfg.Hit1++
			}
			if rank >= 1 && rank <= 5 {
				cfg.Hit5++
			}
			if rank > 0 {
				cfg.MRR += 1 / float64(rank)
			}
		}
	}
	if n := float64(len(cases)); n > 0 {
		for _, cfg := range configs {
			cfg.Hit1 /= n
			cfg.Hit5 /= n
			cfg.MRR /= n
			cfg.AvgMs /= int64(n)
		}
	}
	return res, nil
}

func matchesAny(h *search.Hit, expect []string) bool {
	for _, e := range expect {
		repo, rest, _ := strings.Cut(e, ":")
		p, ln, _ := strings.Cut(rest, ":")
		if h.Repo != repo || !(h.Path == p || strings.HasSuffix(h.Path, "/"+p) || (p != "" && strings.HasPrefix(h.Path, p))) {
			continue
		}
		if n, err := strconv.Atoi(ln); err == nil && (n < h.StartLine || n > h.EndLine) {
			continue
		}
		return true
	}
	return false
}

func (r *evalResult) Text() string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "# %d cases\nconfig\thit@1\thit@5\tMRR\tavg ms\tcost\n", r.Cases)
	for _, c := range r.Configs {
		fmt.Fprintf(tw, "%s\t%.2f\t%.2f\t%.3f\t%d\t$%.4f\n", c.Name, c.Hit1, c.Hit5, c.MRR, c.AvgMs, c.Cost)
	}
	tw.Flush()
	b.WriteString("# misses (rank per config, 0 = not in results):\n")
	for _, row := range r.Rows {
		miss := false
		var parts []string
		for _, c := range r.Configs {
			parts = append(parts, fmt.Sprintf("%s=%d", c.Name, row.Ranks[c.Name]))
			miss = miss || row.Ranks[c.Name] != 1
		}
		if miss {
			fmt.Fprintf(&b, "  %q  %s\n", row.Q, strings.Join(parts, " "))
		}
	}
	return b.String()
}
