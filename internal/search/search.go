// Package search runs the query pipeline:
// mode detection -> shortlist (ripgrep | symbols | BM25) -> heuristic score ->
// optional System One judgment -> threshold -> excerpt packing.
package search

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nikkoxgonzales/code-corpus/internal/chunk"
	"github.com/nikkoxgonzales/code-corpus/internal/index"
	"github.com/nikkoxgonzales/code-corpus/internal/rerank"
	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

const SchemaVersion = 1

const (
	Auto    = "auto"
	Exact   = "exact"
	Regex   = "regex"
	Concept = "concept"
	Symbol  = "symbol"
)

var Modes = []string{Auto, Exact, Regex, Concept, Symbol}

type Options struct {
	Query  string
	Mode   string
	Filter index.Filter
	Limit  int  // 0 = mode default
	Rerank bool // allow the System One judge (concept mode)
	Deep   bool // judge twice as many candidates
	Budget int  // max excerpt bytes across all hits (0 = 8000)
	Lines  int  // max excerpt lines per hit (0 = mode default)
}

type Hit struct {
	Repo       string   `json:"repo"`
	Path       string   `json:"path"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	Symbol     string   `json:"symbol,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Lang       string   `json:"lang,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Score      float64  `json:"score"`
	Relevance  *float64 `json:"relevance,omitempty"`
	Weak       bool     `json:"weak,omitempty"`
	MatchLines []int    `json:"match_lines,omitempty"`
	Excerpt    string   `json:"excerpt,omitempty"`
	Omitted    int      `json:"omitted_lines,omitempty"`

	flags int
	bm25  float64
}

// Loc is the canonical "repo:path:start-end" reference accepted by `corpus show`.
func (h *Hit) Loc() string {
	if h.EndLine > h.StartLine {
		return fmt.Sprintf("%s:%s:%d-%d", h.Repo, h.Path, h.StartLine, h.EndLine)
	}
	return fmt.Sprintf("%s:%s:%d", h.Repo, h.Path, h.StartLine)
}

type Result struct {
	SchemaVersion int      `json:"schema_version"`
	Query         string   `json:"query"`
	Mode          string   `json:"mode"`
	Repos         []string `json:"repos"`
	Reranked      bool     `json:"reranked"`
	Reranker      string   `json:"reranker,omitempty"`
	RerankNote    string   `json:"rerank_note,omitempty"`
	CostUSD       float64  `json:"cost_usd,omitempty"`
	TookMs        int64    `json:"took_ms"`
	Candidates    int      `json:"candidates"`
	Truncated     bool     `json:"truncated"`
	Hits          []*Hit   `json:"hits"`
	Note          string   `json:"note,omitempty"`
	Next          string   `json:"next,omitempty"`
}

type Engine struct {
	DB        *index.DB
	RefDir    string   // references/ directory; repo checkouts live in RefDir/<name>
	Repos     []string // all repo names (used when the filter names none)
	Rerank    *rerank.Config
	RerankOff string // reason reranking is unavailable, when Rerank is nil
	fileCache map[string][]string
}

var identRe = regexp.MustCompile(`^[A-Za-z_$][\w$]*(?:(?:\.|::|#|->)[A-Za-z_$][\w$]*)*$`)

// DetectMode picks a mode for an auto query.
func DetectMode(q string) string {
	q = strings.TrimSpace(q)
	if len(q) >= 2 && q[0] == '"' && q[len(q)-1] == '"' {
		return Exact
	}
	if identRe.MatchString(q) {
		return Symbol
	}
	if len(strings.Fields(q)) <= 2 && strings.ContainsAny(q, `\^$[]()|*+?{}`) {
		if _, err := regexp.Compile(q); err == nil {
			return Regex
		}
	}
	return Concept
}

func (e *Engine) Search(ctx context.Context, o Options) (*Result, error) {
	t0 := time.Now()
	o.Query = strings.TrimSpace(o.Query)
	if o.Query == "" {
		return nil, xerr.New(xerr.User, `corpus search "<query>"`, "empty query")
	}
	if o.Budget <= 0 {
		o.Budget = 8000
	}
	e.fileCache = map[string][]string{}
	res := &Result{SchemaVersion: SchemaVersion, Query: o.Query, Repos: o.Filter.Repos, Hits: []*Hit{}}
	if len(res.Repos) == 0 {
		res.Repos = e.Repos
	}
	if len(res.Repos) == 0 {
		return nil, xerr.New(xerr.NotFound, "corpus add <owner/repo>", "corpus is empty: nothing to search")
	}

	mode := o.Mode
	if mode == "" {
		mode = Auto
	}
	var err error
	if mode == Auto {
		m := DetectMode(o.Query)
		err = e.run(ctx, m, o, res)
		// Fall through to broader modes when the first choice finds nothing.
		if err == nil && len(res.Hits) == 0 && m == Symbol {
			err = e.run(ctx, Exact, o, res)
		}
		if err == nil && len(res.Hits) == 0 && m == Concept {
			if len(strings.Fields(o.Query)) <= 4 {
				err = e.run(ctx, Exact, o, res)
			}
		}
		if res.Mode != m {
			res.Note = m + " search found nothing; showing " + res.Mode + " results"
			res.Mode = m + "->" + res.Mode
		}
	} else {
		err = e.run(ctx, mode, o, res)
	}
	if err != nil {
		return nil, err
	}
	e.pack(res, o)
	res.Next = next(res, o)
	res.TookMs = time.Since(t0).Milliseconds()
	return res, nil
}

func (e *Engine) run(ctx context.Context, mode string, o Options, res *Result) error {
	res.Mode = mode
	switch mode {
	case Exact:
		q := o.Query
		if len(q) >= 2 && q[0] == '"' && q[len(q)-1] == '"' {
			q = q[1 : len(q)-1]
		}
		return e.grep(ctx, q, true, o, res)
	case Regex:
		if _, err := regexp.Compile(o.Query); err != nil {
			return xerr.New(xerr.User, "escape special characters or use --mode exact", "invalid regex: %v", err)
		}
		return e.grep(ctx, o.Query, false, o, res)
	case Symbol:
		return e.symbols(o, res)
	case Concept:
		return e.concept(ctx, o, res)
	}
	return xerr.New(xerr.User, "modes: "+strings.Join(Modes, ", "), "unknown mode %q", mode)
}

// ---- concept ----

func (e *Engine) concept(ctx context.Context, o Options, res *Result) error {
	terms := index.QueryTerms(o.Query)
	// A query naming a repo ("how does ky retry") boosts that repo; the name is not a search term.
	mentioned := map[string]bool{}
	if len(o.Filter.Repos) == 0 {
		kept := terms[:0:0]
		for _, t := range terms {
			isRepo := false
			for _, r := range e.Repos {
				if strings.EqualFold(r, t) {
					mentioned[r], isRepo = true, true
				}
			}
			if !isRepo {
				kept = append(kept, t)
			}
		}
		if len(kept) > 0 {
			terms = kept
		}
	}
	if len(terms) == 0 {
		return xerr.New(xerr.User, "add specific words, e.g. identifiers or domain terms", "query %q has no searchable terms", o.Query)
	}
	cands, err := e.DB.BM25(index.MatchExpr(terms), o.Filter, 150)
	if err != nil {
		return err
	}
	res.Candidates = len(cands)
	if len(cands) == 0 {
		return nil
	}
	maxB := cands[0].BM25
	for _, c := range cands {
		maxB = math.Max(maxB, c.BM25)
	}
	var hits []*Hit
	for _, c := range cands {
		h := candHit(c)
		h.bm25 = c.BM25
		h.Score = conceptScore(c, maxB, terms, o.Query)
		if mentioned[c.Repo] {
			h.Score *= 2
		}
		hits = append(hits, h)
	}
	sortHits(hits)
	hits = capPerFile(hits, 3)

	limit := o.Limit
	if limit <= 0 {
		limit = 8
	}
	if !o.Rerank || e.Rerank == nil {
		if o.Rerank && e.Rerank == nil {
			res.RerankNote = e.RerankOff
		} else if !o.Rerank {
			res.RerankNote = "disabled by --no-rerank"
		}
		res.Hits = hits[:min(limit, len(hits))]
		e.excerptConcept(res.Hits, terms, o)
		return nil
	}

	k := rerank.BatchSize
	if o.Deep {
		k *= 2
	}
	pool := hits[:min(k, len(hits))]
	items := make([]rerank.Item, len(pool))
	for i, h := range pool {
		items[i] = rerank.Item{ID: fmt.Sprint(i), Header: h.Loc() + " " + h.Kind + " " + h.Symbol, Text: e.judgeText(h)}
	}
	jr, err := e.Rerank.Judge(ctx, o.Query, items)
	if err != nil {
		res.RerankNote = "rerank failed (" + err.Error() + "); keyword order"
		res.Hits = hits[:min(limit, len(hits))]
		e.excerptConcept(res.Hits, terms, o)
		return nil
	}
	res.Reranked = true
	res.Reranker = jr.Provider + ":" + jr.Model
	res.CostUSD = jr.CostUSD
	maxS := 0.0
	for _, h := range pool {
		maxS = math.Max(maxS, h.Score)
	}
	var kept, weak []*Hit
	for i, h := range pool {
		p, ok := jr.Scores[fmt.Sprint(i)]
		if !ok {
			continue
		}
		pp := math.Round(p*100) / 100
		h.Relevance = &pp
		// Order by the judge's probability; keyword score only breaks near-ties.
		h.Score = pp + blend*(h.Score/maxS)
		if mentioned[h.Repo] {
			h.Score += 0.3
		}
		if p >= 0.5 {
			kept = append(kept, h)
		} else {
			weak = append(weak, h)
		}
	}
	sortHits(kept)
	if len(kept) == 0 {
		sortHits(weak)
		for _, h := range weak[:min(3, len(weak))] {
			h.Weak = true
			kept = append(kept, h)
		}
	}
	res.Hits = kept[:min(limit, len(kept))]
	if len(res.Hits) > 0 && res.Hits[0].Weak {
		return nil // nothing relevant: locations only, no excerpt tokens
	}
	e.excerptConcept(res.Hits, terms, o)
	return nil
}

func candHit(c index.Cand) *Hit {
	return &Hit{Repo: c.Repo, Path: c.Path, StartLine: c.Start, EndLine: c.End, Symbol: c.Symbol,
		Kind: c.Kind, Lang: c.Lang, Tags: tags(c.Flags), flags: c.Flags}
}

func tags(flags int) []string {
	var t []string
	if flags&index.FlagTest != 0 {
		t = append(t, "test")
	}
	if flags&index.FlagVendor != 0 {
		t = append(t, "vendor")
	}
	if flags&index.FlagGenerated != 0 {
		t = append(t, "generated")
	}
	if flags&index.FlagDoc != 0 {
		t = append(t, "doc")
	}
	return t
}

var testWords = regexp.MustCompile(`(?i)\b(test|tests|testing|spec|mock|fixture)`)
var docWords = regexp.MustCompile(`(?i)\b(doc|docs|readme|guide|example|examples|usage|tutorial)`)

func penalty(flags int, q string) float64 {
	p := 1.0
	if flags&index.FlagTest != 0 && !testWords.MatchString(q) {
		p *= 0.5
	}
	if flags&index.FlagVendor != 0 {
		p *= 0.4
	}
	if flags&index.FlagGenerated != 0 {
		p *= 0.4
	}
	if flags&index.FlagDoc != 0 && !docWords.MatchString(q) {
		p *= 0.75
	}
	return p
}

func conceptScore(c index.Cand, maxB float64, terms []string, q string) float64 {
	s := c.BM25 / maxB
	sym, p := strings.ToLower(c.Symbol), strings.ToLower(c.Path)
	var inSym, inPath float64
	for _, t := range terms {
		if sym != "" && strings.Contains(sym, t) {
			inSym++
		}
		if strings.Contains(p, t) {
			inPath++
		}
	}
	n := float64(len(terms))
	s *= 1 + 0.5*inSym/n + 0.25*inPath/n
	switch c.Kind {
	case "block":
		s *= 0.9
	case "func", "method", "class", "type", "impl", "module":
		s *= 1.1
	}
	return s * penalty(c.Flags, q)
}

func sortHits(h []*Hit) {
	sort.SliceStable(h, func(i, j int) bool {
		if h[i].Score != h[j].Score {
			return h[i].Score > h[j].Score
		}
		if h[i].Repo != h[j].Repo {
			return h[i].Repo < h[j].Repo
		}
		if h[i].Path != h[j].Path {
			return h[i].Path < h[j].Path
		}
		return h[i].StartLine < h[j].StartLine
	})
}

func capPerFile(h []*Hit, n int) []*Hit {
	seen := map[string]int{}
	var out []*Hit
	for _, x := range h {
		k := x.Repo + ":" + x.Path
		if seen[k] < n {
			seen[k]++
			out = append(out, x)
		}
	}
	return out
}

// ---- symbols ----

func (e *Engine) symbols(o Options, res *Result) error {
	limit := o.Limit
	if limit <= 0 {
		limit = 10
	}
	syms, err := e.DB.FindSymbols(o.Query, o.Filter, 200)
	if err != nil {
		return err
	}
	res.Candidates = len(syms)
	// Keep only the best match class present (exact beats prefix beats contains).
	best := 4
	for _, s := range syms {
		best = min(best, max(s.Match, 1))
	}
	if best > 1 && len(syms) > 0 {
		res.Note = "no definition named exactly " + o.Query + "; showing partial name matches"
	}
	var hits []*Hit
	for _, s := range syms {
		if max(s.Match, 1) > best && len(hits) >= limit/2 {
			continue
		}
		h := &Hit{Repo: s.Repo, Path: s.Path, StartLine: s.Start, EndLine: s.End, Symbol: s.Name, Kind: s.Kind,
			Lang: s.Lang, Tags: tags(s.Flags), flags: s.Flags, MatchLines: []int{s.Line}}
		h.Score = []float64{1, 0.9, 0.6, 0.3}[s.Match] * penalty(s.Flags, o.Query)
		hits = append(hits, h)
	}
	sortHits(hits)
	res.Hits = hits[:min(limit, len(hits))]
	lines := o.Lines
	if lines <= 0 {
		lines = 14
	}
	for _, h := range res.Hits {
		fl := e.file(h.Repo, h.Path)
		def := h.MatchLines[0]
		start := h.StartLine
		if def-start > 6 { // skip long leading comments
			start = def - 3
		}
		end := min(h.EndLine, start+lines-1)
		h.Excerpt = render(fl, start, end, map[int]bool{def: true})
		h.Omitted = max(0, h.EndLine-end)
	}
	return nil
}

// ---- excerpts ----

func (e *Engine) file(repo, p string) []string {
	k := repo + "/" + p
	if l, ok := e.fileCache[k]; ok {
		return l
	}
	b, err := os.ReadFile(filepath.Join(e.RefDir, repo, filepath.FromSlash(p)))
	var l []string
	if err == nil {
		l = chunk.SplitLines(string(b))
	}
	e.fileCache[k] = l
	return l
}

func (e *Engine) judgeText(h *Hit) string {
	fl := e.file(h.Repo, h.Path)
	var b strings.Builder
	for n := h.StartLine; n <= min(h.EndLine, h.StartLine+79) && n <= len(fl); n++ {
		l := fl[n-1]
		if len(l) > 200 {
			l = l[:200]
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// render formats lines [start,end] (1-based) as "  41| code", marking lines in mark with '>'.
func render(fl []string, start, end int, mark map[int]bool) string {
	var b strings.Builder
	w := len(fmt.Sprint(end))
	for n := max(start, 1); n <= end && n <= len(fl); n++ {
		m := ' '
		if mark[n] {
			m = '>'
		}
		l := strings.TrimRight(fl[n-1], " \t")
		if len(l) > 240 {
			l = l[:240] + "..."
		}
		fmt.Fprintf(&b, "%c%*d| %s\n", m, w, n, l)
	}
	return b.String()
}

// excerptConcept shows a chunk's head plus the window densest in query terms.
func (e *Engine) excerptConcept(hits []*Hit, terms []string, o Options) {
	maxLines := o.Lines
	if maxLines <= 0 {
		maxLines = 30
	}
	for _, h := range hits {
		fl := e.file(h.Repo, h.Path)
		end := min(h.EndLine, len(fl))
		n := end - h.StartLine + 1
		if n <= maxLines {
			h.Excerpt = render(fl, h.StartLine, end, nil)
			continue
		}
		head := 6
		win := maxLines - head
		bestAt, bestScore := h.StartLine+head, -1
		score := func(ln int) int {
			l := strings.ToLower(fl[ln-1])
			s := 0
			for _, t := range terms {
				if strings.Contains(l, t) {
					s++
				}
			}
			return s
		}
		for s := h.StartLine + head; s+win-1 <= end; s++ {
			sum := 0
			for ln := s; ln < s+win; ln++ {
				sum += score(ln)
			}
			if sum > bestScore {
				bestAt, bestScore = s, sum
			}
			if s-h.StartLine > 400 { // bound work on huge chunks
				break
			}
		}
		out := render(fl, h.StartLine, h.StartLine+head-1, nil)
		if bestAt > h.StartLine+head {
			out += "     ...\n"
		}
		out += render(fl, bestAt, bestAt+win-1, nil)
		h.Excerpt = out
		h.Omitted = n - maxLines
	}
}

// pack enforces the byte budget: later hits keep their location but lose excerpts.
func (e *Engine) pack(res *Result, o Options) {
	used := 0
	for i, h := range res.Hits {
		if i > 0 && used+len(h.Excerpt) > o.Budget {
			if h.Excerpt != "" {
				res.Truncated = true
			}
			h.Excerpt = ""
			h.Omitted = 0
			continue
		}
		used += len(h.Excerpt)
	}
}

func next(res *Result, o Options) string {
	q := strings.ReplaceAll(o.Query, `"`, `\"`)
	if len(res.Hits) == 0 {
		switch {
		case strings.HasPrefix(res.Mode, Concept) || strings.HasSuffix(res.Mode, Concept):
			return fmt.Sprintf(`corpus search "%s" --mode exact   (or rephrase with identifiers/domain terms)`, q)
		case strings.HasSuffix(res.Mode, Symbol):
			return fmt.Sprintf(`corpus search "%s" --mode concept`, q)
		default:
			return fmt.Sprintf(`corpus search "%s" --mode concept`, q)
		}
	}
	h := res.Hits[0]
	if h.Weak {
		return fmt.Sprintf(`nothing clearly relevant. try: corpus search "%s" --deep, rephrase with identifiers, or --mode exact`, q)
	}
	if h.Omitted > 0 || h.Excerpt == "" {
		return "corpus show " + h.Loc()
	}
	if res.Truncated {
		return "corpus show " + res.Hits[1].Loc()
	}
	return fmt.Sprintf("corpus show %s:%s:%d-%d", h.Repo, h.Path, max(1, h.StartLine-20), h.EndLine+40)
}

// Text renders the result for agents: a header line, one block per hit, a next hint.
func (r *Result) Text() string {
	var b strings.Builder
	info := []string{r.Mode}
	if r.Reranked {
		info = append(info, fmt.Sprintf("reranked by %s ($%.5f)", r.Reranker, r.CostUSD))
	} else if r.RerankNote != "" && strings.Contains(r.Mode, Concept) {
		info = append(info, "keyword-ranked: "+r.RerankNote)
	}
	info = append(info, plural(len(r.Repos), "repo"), fmt.Sprintf("%dms", r.TookMs))
	fmt.Fprintf(&b, "# %s for %q | %s\n", plural(len(r.Hits), "result"), r.Query, strings.Join(info, " | "))
	for i, h := range r.Hits {
		fmt.Fprintf(&b, "[%d] %s", i+1, h.Loc())
		if h.Kind != "" && h.Kind != "block" && h.Symbol != "" {
			fmt.Fprintf(&b, "  %s %s", h.Kind, h.Symbol)
		} else if h.Symbol != "" {
			fmt.Fprintf(&b, "  in %s", h.Symbol)
		}
		if h.Relevance != nil {
			fmt.Fprintf(&b, "  p=%.2f", *h.Relevance)
		}
		if h.Weak {
			b.WriteString(" weak")
		}
		for _, t := range h.Tags {
			b.WriteString(" [" + t + "]")
		}
		b.WriteByte('\n')
		b.WriteString(h.Excerpt)
		if h.Omitted > 0 {
			fmt.Fprintf(&b, "     ... +%d lines\n", h.Omitted)
		}
	}
	if r.Note != "" {
		b.WriteString("# note: " + r.Note + "\n")
	}
	if r.Truncated {
		b.WriteString("# output budget reached: later results show location only\n")
	}
	if r.Next != "" {
		b.WriteString("# next: " + r.Next + "\n")
	}
	return b.String()
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}

// blend weights the keyword score against the judge's probability (tuned with corpus eval).
var blend = func() float64 {
	if f, err := strconv.ParseFloat(os.Getenv("CORPUS_BLEND"), 64); err == nil {
		return f
	}
	return 0.4
}()
