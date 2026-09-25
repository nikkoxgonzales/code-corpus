package app

import (
	"context"
	"sort"
	"strings"

	"github.com/nikkoxgonzales/code-corpus/internal/chunk"
	"github.com/nikkoxgonzales/code-corpus/internal/index"
	"github.com/nikkoxgonzales/code-corpus/internal/search"
	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

type SearchArgs struct {
	Query      string
	Mode       string
	Repos      []string
	Categories []string
	Langs      []string
	Path       string
	Limit      int
	Budget     int
	Lines      int
	NoRerank   bool
	Deep       bool
}

func (a *App) Search(ctx context.Context, s SearchArgs) (*search.Result, error) {
	if s.Mode != "" {
		ok := false
		for _, m := range search.Modes {
			ok = ok || m == s.Mode
		}
		if !ok {
			return nil, xerr.New(xerr.User, "modes: "+strings.Join(search.Modes, ", "), "unknown mode %q", s.Mode)
		}
	}
	var langs []string
	for _, l := range s.Langs {
		for _, x := range strings.Split(l, ",") {
			if x = strings.ToLower(strings.TrimSpace(x)); x == "" {
				continue
			}
			if !chunk.KnownLang(x) {
				all := chunk.Langs()
				sort.Strings(all)
				return nil, xerr.New(xerr.User, "languages: "+strings.Join(all, ", "), "unknown language %q", x)
			}
			langs = append(langs, x)
		}
	}
	repos, err := a.ResolveRepos(s.Repos)
	if err != nil {
		return nil, err
	}
	cats, err := ParseCategories(s.Categories)
	if err != nil {
		return nil, err
	}
	catRepos, err := a.CategoryRepos(cats)
	if err != nil {
		return nil, err
	}
	repos = append(repos, catRepos...)
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	return e.Search(ctx, search.Options{
		Query:  s.Query,
		Mode:   s.Mode,
		Filter: index.Filter{Repos: repos, Langs: langs, Path: s.Path},
		Limit:  s.Limit,
		Rerank: !s.NoRerank,
		Deep:   s.Deep,
		Budget: s.Budget,
		Lines:  s.Lines,
	})
}
