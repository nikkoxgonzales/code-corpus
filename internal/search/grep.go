package search

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nikkoxgonzales/code-corpus/internal/chunk"
	"github.com/nikkoxgonzales/code-corpus/internal/index"
	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

const maxMatches = 5000

type match struct {
	repo, path string
	line       int
}

// grep runs ripgrep (or git grep as a fallback) and groups matches by enclosing chunk.
func (e *Engine) grep(ctx context.Context, pat string, fixed bool, o Options, res *Result) error {
	ms, err := e.rg(ctx, pat, fixed, o)
	if err != nil {
		return err
	}
	if o.Filter.Path != "" {
		re := globRegex(index.GlobPattern(o.Filter.Path))
		kept := ms[:0]
		for _, m := range ms {
			if re.MatchString(m.path) {
				kept = append(kept, m)
			}
		}
		ms = kept
	}
	res.Candidates = len(ms)

	type group struct {
		h     *Hit
		lines []int
	}
	groups := map[string]*group{}
	var order []string
	chunkCache := map[string][]index.Cand{}
	for _, m := range ms {
		fk := m.repo + ":" + m.path
		cs, ok := chunkCache[fk]
		if !ok {
			cs, _ = e.DB.FileChunks(m.repo, m.path)
			chunkCache[fk] = cs
		}
		var h *Hit
		i := sort.Search(len(cs), func(i int) bool { return cs[i].End >= m.line })
		if i < len(cs) && cs[i].Start <= m.line {
			h = candHit(cs[i])
		} else {
			flags := index.PathFlags(m.path)
			h = &Hit{Repo: m.repo, Path: m.path, StartLine: max(1, m.line-5), EndLine: m.line + 5,
				Kind: "match", Lang: chunk.Lang(m.path), Tags: tags(flags), flags: flags}
		}
		k := fk + ":" + strconv.Itoa(h.StartLine)
		g := groups[k]
		if g == nil {
			g = &group{h: h}
			groups[k] = g
			order = append(order, k)
		}
		g.lines = append(g.lines, m.line)
	}

	lq := strings.ToLower(pat)
	var hits []*Hit
	for _, k := range order {
		g := groups[k]
		h := g.h
		h.MatchLines = g.lines
		s := 1 + math.Log(1+float64(len(g.lines)))
		if sym := strings.ToLower(h.Symbol); sym != "" && fixed {
			if sym == lq {
				s *= 3
			} else if strings.Contains(sym, lq) {
				s *= 2
			}
		}
		if fixed && strings.Contains(strings.ToLower(path.Base(h.Path)), lq) {
			s *= 1.3
		}
		h.Score = s * penalty(h.flags, pat)
		hits = append(hits, h)
	}
	sortHits(hits)
	limit := o.Limit
	if limit <= 0 {
		limit = 10
	}
	res.Hits = hits[:min(limit, len(hits))]

	maxLines := o.Lines
	if maxLines <= 0 {
		maxLines = 15
	}
	for _, h := range res.Hits {
		e.excerptMatches(h, maxLines)
	}
	return nil
}

// excerptMatches shows up to 3 windows of +-2 lines around matching lines.
func (e *Engine) excerptMatches(h *Hit, maxLines int) {
	fl := e.file(h.Repo, h.Path)
	mark := map[int]bool{}
	for _, l := range h.MatchLines {
		mark[l] = true
	}
	type win struct{ a, b int }
	var ws []win
	for _, l := range h.MatchLines {
		a, b := max(h.StartLine, l-2), min(h.EndLine, l+2)
		if n := len(ws); n > 0 && a <= ws[n-1].b+1 {
			ws[n-1].b = max(ws[n-1].b, b)
			continue
		}
		ws = append(ws, win{a, b})
	}
	var out strings.Builder
	used := 0
	for i, w := range ws {
		if i >= 3 || used >= maxLines {
			break
		}
		b := min(w.b, w.a+maxLines-used-1)
		if i > 0 {
			out.WriteString("     ...\n")
		}
		out.WriteString(render(fl, w.a, b, mark))
		used += b - w.a + 1
	}
	h.Excerpt = out.String()
}

func (e *Engine) repos(o Options) []string {
	if len(o.Filter.Repos) > 0 {
		return o.Filter.Repos
	}
	return e.Repos
}

type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		LineNumber int `json:"line_number"`
	} `json:"data"`
}

func (e *Engine) rg(ctx context.Context, pat string, fixed bool, o Options) ([]match, error) {
	repos := e.repos(o)
	if _, err := exec.LookPath("rg"); err != nil {
		return e.gitGrep(ctx, pat, fixed, o, repos)
	}
	args := []string{"--json", "--line-number", "--max-count", "50", "--max-filesize", "1M", "--max-columns", "400", "--smart-case",
		"-g", "!node_modules", "-g", "!*.min.*", "-g", "!*.lock", "-g", "!package-lock.json", "-g", "!go.sum"}
	if fixed {
		args = append(args, "--fixed-strings")
	}
	for _, l := range o.Filter.Langs {
		for _, x := range chunk.Exts(l) {
			args = append(args, "-g", "*"+x)
		}
	}
	args = append(args, "-e", pat, "--")
	args = append(args, repos...)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = e.RefDir
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var ms []match
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev rgEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "match" || ev.Data.Path.Text == "" {
			continue
		}
		p := filepath.ToSlash(ev.Data.Path.Text)
		repo, rel, ok := strings.Cut(p, "/")
		if !ok {
			continue
		}
		ms = append(ms, match{repo: repo, path: rel, line: ev.Data.LineNumber})
		if len(ms) >= maxMatches {
			cancel()
			break
		}
	}
	cmd.Wait() // exit 1 = no matches; errors surface as no results
	return ms, nil
}

func (e *Engine) gitGrep(ctx context.Context, pat string, fixed bool, o Options, repos []string) ([]match, error) {
	var ms []match
	for _, r := range repos {
		args := []string{"grep", "-n", "-I", "--no-color", "--full-name"}
		if fixed {
			args = append(args, "-F")
		} else {
			args = append(args, "-E")
		}
		if strings.ToLower(pat) == pat {
			args = append(args, "-i")
		}
		args = append(args, "-e", pat)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = filepath.Join(e.RefDir, r)
		out, err := cmd.Output()
		if err != nil && len(out) == 0 {
			continue
		}
		for _, ln := range strings.Split(string(out), "\n") {
			p, rest, ok := strings.Cut(ln, ":")
			if !ok {
				continue
			}
			num, _, _ := strings.Cut(rest, ":")
			n, err := strconv.Atoi(num)
			if err != nil {
				continue
			}
			ms = append(ms, match{repo: r, path: p, line: n})
			if len(ms) >= maxMatches {
				return ms, nil
			}
		}
	}
	if len(ms) == 0 && len(repos) == 0 {
		return nil, xerr.New(xerr.NotFound, "corpus add <owner/repo>", "no repos to search")
	}
	return ms, nil
}

// globRegex matches the same paths as an SQLite GLOB pattern.
func globRegex(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range g {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
