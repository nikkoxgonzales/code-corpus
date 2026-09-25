package app

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

var catRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ParseCategories splits comma-separated values into lowercase category names.
func ParseCategories(in []string) ([]string, error) {
	var out []string
	for _, s := range in {
		for _, c := range strings.Split(s, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c == "" {
				continue
			}
			if !catRe.MatchString(c) {
				return nil, xerr.New(xerr.User, "use lowercase letters, digits and dashes, e.g. web-framework", "invalid category %q", c)
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// CategoryRepos expands categories to repo names; an unknown category is an error.
func (a *App) CategoryRepos(cats []string) ([]string, error) {
	var out []string
	for _, c := range cats {
		names := a.M.InCategory(c)
		if len(names) == 0 {
			return nil, xerr.New(xerr.NotFound, "corpus categories", "no repos in category %q", c)
		}
		out = append(out, names...)
	}
	return out, nil
}

type TagResult struct {
	Categories []string `json:"categories"`
	Removed    bool     `json:"removed"`
	Repos      []string `json:"repos"`
}

func (r *TagResult) Text() string {
	verb := "tagged"
	if r.Removed {
		verb = "untagged"
	}
	return fmt.Sprintf("%s %s: %s\n# next: corpus list --category %s\n", verb, strings.Join(r.Categories, ","), strings.Join(r.Repos, " "), r.Categories[0])
}

// Tag adds or removes categories on repos.
func (a *App) Tag(cats []string, names []string, remove bool) (*TagResult, error) {
	cs, err := ParseCategories(cats)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 || len(names) == 0 {
		return nil, xerr.New(xerr.User, "corpus tag <category[,category]> <repo> ...", "need a category and at least one repo")
	}
	repos, err := a.ResolveRepos(names)
	if err != nil {
		return nil, err
	}
	for _, n := range repos {
		a.M.Tag(n, cs, remove)
	}
	if err := a.M.Save(); err != nil {
		return nil, err
	}
	return &TagResult{Categories: cs, Removed: remove, Repos: repos}, nil
}

type CategoryItem struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Repos []string `json:"repos"`
}

type CategoriesResult struct {
	Categories    []*CategoryItem `json:"categories"`
	Uncategorized []string        `json:"uncategorized"`
}

func (a *App) CategoriesList() (*CategoriesResult, error) {
	res := &CategoriesResult{Categories: []*CategoryItem{}, Uncategorized: []string{}}
	counts := a.M.Categories()
	var names []string
	for c := range counts {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		res.Categories = append(res.Categories, &CategoryItem{Name: c, Count: counts[c], Repos: a.M.InCategory(c)})
	}
	for _, n := range a.M.Names() {
		if len(a.M.Get(n).Categories) == 0 {
			res.Uncategorized = append(res.Uncategorized, n)
		}
	}
	return res, nil
}

func (r *CategoriesResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d categories\n", len(r.Categories))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, c := range r.Categories {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", c.Name, c.Count, strings.Join(c.Repos, " "))
	}
	tw.Flush()
	if len(r.Uncategorized) > 0 {
		fmt.Fprintf(&b, "# uncategorized: %s\n# fix: corpus tag <category> <repo> ...\n", strings.Join(r.Uncategorized, " "))
	} else if len(r.Categories) > 0 {
		fmt.Fprintf(&b, "# next: corpus search \"<question>\" --category %s\n", r.Categories[0].Name)
	}
	return b.String()
}
