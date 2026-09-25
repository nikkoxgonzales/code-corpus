// Package app implements corpus operations shared by the CLI and the MCP server.
// Every operation returns a Result whose Text() is the agent-facing rendering.
package app

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nikkoxgonzales/code-corpus/internal/gitops"
	"github.com/nikkoxgonzales/code-corpus/internal/index"
	"github.com/nikkoxgonzales/code-corpus/internal/manifest"
	"github.com/nikkoxgonzales/code-corpus/internal/rerank"
	"github.com/nikkoxgonzales/code-corpus/internal/search"
	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

const Version = "0.1.0"

type App struct {
	Root    string // directory containing references/
	RefDir  string // Root/references
	DataDir string // Root/references/.corpus
	M       *manifest.Manifest

	dbOnce sync.Once
	db     *index.DB
	dbErr  error
}

// FindRoot resolves the corpus root: --root, $CORPUS_ROOT, the nearest ancestor of
// the working directory holding references/.corpus, then the executable's directory.
func FindRoot(flagRoot string) (string, error) {
	if flagRoot != "" {
		return filepath.Abs(flagRoot)
	}
	if r := os.Getenv("CORPUS_ROOT"); r != "" {
		return filepath.Abs(r)
	}
	if wd, err := os.Getwd(); err == nil {
		for d := wd; ; d = filepath.Dir(d) {
			if isDir(filepath.Join(d, "references", ".corpus")) {
				return d, nil
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
		if d := filepath.Dir(exe); isDir(filepath.Join(d, "references")) {
			return d, nil
		}
	}
	return "", xerr.New(xerr.User, "set CORPUS_ROOT to the directory that should hold references/ (or pass --root)", "corpus root not found")
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func Open(root string) (*App, error) {
	a := &App{Root: root, RefDir: filepath.Join(root, "references")}
	a.DataDir = filepath.Join(a.RefDir, ".corpus")
	if err := os.MkdirAll(a.DataDir, 0o755); err != nil {
		return nil, err
	}
	m, err := manifest.Load(filepath.Join(a.DataDir, "corpus.json"))
	if err != nil {
		return nil, err
	}
	a.M = m
	return a, nil
}

func (a *App) DB() (*index.DB, error) {
	a.dbOnce.Do(func() { a.db, a.dbErr = index.Open(filepath.Join(a.DataDir, "index.db")) })
	return a.db, a.dbErr
}

func (a *App) Close() {
	if a.db != nil {
		a.db.Close()
	}
}

func (a *App) Engine() (*search.Engine, error) {
	db, err := a.DB()
	if err != nil {
		return nil, err
	}
	rc, why := rerank.FromEnv()
	return &search.Engine{DB: db, RefDir: a.RefDir, Repos: a.M.Names(), Rerank: rc, RerankOff: why}, nil
}

// ResolveRepos maps user-supplied names (possibly comma-separated) to repo names.
func (a *App) ResolveRepos(in []string) ([]string, error) {
	var out []string
	for _, s := range in {
		for _, n := range strings.Split(s, ",") {
			if n = strings.TrimSpace(n); n == "" {
				continue
			}
			r, err := a.M.Resolve(n)
			if err != nil {
				return nil, err
			}
			out = append(out, r.Name)
		}
	}
	return out, nil
}

func (a *App) dir(name string) string { return filepath.Join(a.RefDir, name) }

// ---- add ----

var (
	shortRe   = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][\w.-]*$`)
	treeRefRe = regexp.MustCompile(`^(https?://[^/]+/[^/]+/[^/]+)/tree/(.+)$`)
)

// ParseSource accepts owner/repo, github.com/owner/repo, https or ssh URLs,
// with an optional @ref suffix or /tree/<ref> path.
func ParseSource(s string) (url, owner, name, ref string, err error) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "@"); i > strings.LastIndex(s, "/") && i > 0 {
		s, ref = s[:i], s[i+1:]
	}
	if m := treeRefRe.FindStringSubmatch(s); m != nil {
		s, ref = m[1], m[2]
	}
	switch {
	case shortRe.MatchString(s) && !strings.HasPrefix(s, "github.com"):
		url = "https://github.com/" + s
	case strings.HasPrefix(s, "github.com/") || strings.HasPrefix(s, "gitlab.com/") || strings.HasPrefix(s, "bitbucket.org/"):
		url = "https://" + s
	case strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "ssh://"):
		url = s
	default:
		return "", "", "", "", xerr.New(xerr.User, "use owner/repo, a GitHub URL, or owner/repo@tag", "cannot parse source %q", s)
	}
	url = strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	parts := strings.FieldsFunc(url, func(r rune) bool { return r == '/' || r == ':' })
	if len(parts) < 2 {
		return "", "", "", "", xerr.New(xerr.User, "use owner/repo", "cannot parse source %q", s)
	}
	name = strings.ToLower(parts[len(parts)-1])
	owner = strings.ToLower(parts[len(parts)-2])
	return url, owner, name, ref, nil
}

type AddItem struct {
	Source  string      `json:"source"`
	Name    string      `json:"name,omitempty"`
	Status  string      `json:"status"` // added | exists | error
	Ref     string      `json:"ref,omitempty"`
	SHA     string      `json:"sha,omitempty"`
	Files   int         `json:"files,omitempty"`
	Chunks  int         `json:"chunks,omitempty"`
	Seconds float64     `json:"seconds,omitempty"`
	Error   *xerr.Error `json:"error,omitempty"`
}

type AddResult struct {
	Items []*AddItem `json:"items"`
	Next  string     `json:"next,omitempty"`
}

func (a *App) Add(sources []string, name string) (*AddResult, error) {
	if len(sources) == 0 {
		return nil, xerr.New(xerr.User, "corpus add owner/repo [owner/repo@tag ...]", "no source given")
	}
	if name != "" && len(sources) > 1 {
		return nil, xerr.New(xerr.User, "add repos one at a time when using --name", "--name needs exactly one source")
	}
	if err := gitops.Available(); err != nil {
		return nil, xerr.New(xerr.User, "install git", "%v", err)
	}
	db, err := a.DB()
	if err != nil {
		return nil, err
	}
	res := &AddResult{}
	var mu sync.Mutex
	reserved := map[string]bool{}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, src := range sources {
		it := &AddItem{Source: src}
		res.Items = append(res.Items, it)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			fail := func(err error) { it.Status, it.Error = "error", xerr.As(err) }

			url, owner, n, ref, err := ParseSource(src)
			if err != nil {
				fail(err)
				return
			}
			if ex := a.M.ByURL(url); ex != nil {
				it.Status, it.Name, it.Ref, it.SHA = "exists", ex.Name, ex.Ref, ex.SHA
				return
			}
			mu.Lock()
			if name != "" {
				n = name
			} else if a.M.Get(n) != nil || reserved[n] {
				n = owner + "-" + n
			}
			if !nameRe.MatchString(n) || a.M.Get(n) != nil || reserved[n] {
				mu.Unlock()
				fail(xerr.New(xerr.User, "pass --name <unique-name>", "name %q is invalid or taken", n))
				return
			}
			reserved[n] = true
			mu.Unlock()
			it.Name = n
			dir := a.dir(n)
			if isDir(dir) {
				fail(xerr.New(xerr.User, "delete "+dir+" or pass --name <other>", "directory references/%s already exists but is not tracked", n))
				return
			}
			tracked, pinned, err := gitops.Clone(url, ref, dir)
			if err != nil {
				fail(err)
				return
			}
			sha, err := gitops.HeadSHA(dir)
			if err != nil {
				fail(err)
				return
			}
			ents, err := gitops.LsFiles(dir)
			if err != nil {
				fail(err)
				return
			}
			st, err := db.IndexRepo(n, dir, ents)
			if err != nil {
				fail(err)
				return
			}
			now := time.Now().UTC().Truncate(time.Second)
			a.M.Put(&manifest.Repo{Name: n, URL: url, Ref: tracked, Pinned: pinned, SHA: sha, AddedAt: now, UpdatedAt: now})
			it.Status, it.Ref, it.SHA, it.Files, it.Chunks = "added", tracked, sha, st.Files, st.Chunks
			it.Seconds = round1(time.Since(t0).Seconds())
		}()
	}
	wg.Wait()
	if err := a.M.Save(); err != nil {
		return nil, err
	}
	for _, it := range res.Items {
		if it.Status != "error" {
			res.Next = fmt.Sprintf(`corpus search "<question>" --repo %s   (or: corpus tree %s)`, it.Name, it.Name)
			break
		}
	}
	return res, nil
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func (r *AddResult) ExitCode() int { return itemsExit(len(r.Items), r.errs()) }

func (r *AddResult) errs() []*xerr.Error {
	var e []*xerr.Error
	for _, it := range r.Items {
		if it.Error != nil {
			e = append(e, it.Error)
		}
	}
	return e
}

func itemsExit(n int, errs []*xerr.Error) int {
	switch {
	case len(errs) == 0:
		return xerr.OK
	case len(errs) < n:
		return xerr.Partial
	default:
		return errs[0].Code
	}
}

// ---- update ----

type UpdateItem struct {
	Name    string      `json:"name"`
	Status  string      `json:"status"` // updated | current | pinned | recloned | error
	OldSHA  string      `json:"old_sha,omitempty"`
	NewSHA  string      `json:"new_sha,omitempty"`
	Added   int         `json:"files_added,omitempty"`
	Changed int         `json:"files_changed,omitempty"`
	Removed int         `json:"files_removed,omitempty"`
	Seconds float64     `json:"seconds,omitempty"`
	Error   *xerr.Error `json:"error,omitempty"`
}

type UpdateResult struct {
	Items []*UpdateItem `json:"items"`
}

func (r *UpdateResult) ExitCode() int {
	var e []*xerr.Error
	for _, it := range r.Items {
		if it.Error != nil {
			e = append(e, it.Error)
		}
	}
	return itemsExit(len(r.Items), e)
}

func (a *App) Update(names []string) (*UpdateResult, error) {
	if len(names) == 0 {
		names = a.M.Names()
	} else {
		var err error
		if names, err = a.ResolveRepos(names); err != nil {
			return nil, err
		}
	}
	if len(names) == 0 {
		return nil, xerr.New(xerr.NotFound, "corpus add <owner/repo>", "corpus is empty: nothing to update")
	}
	db, err := a.DB()
	if err != nil {
		return nil, err
	}
	res := &UpdateResult{}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, n := range names {
		r := a.M.Get(n)
		it := &UpdateItem{Name: n, OldSHA: r.SHA}
		res.Items = append(res.Items, it)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			fail := func(err error) { it.Status, it.Error = "error", xerr.As(err) }
			dir := a.dir(n)
			switch {
			case !isDir(filepath.Join(dir, ".git")):
				os.RemoveAll(dir)
				ref := r.Ref
				if _, _, err := gitops.Clone(r.URL, ref, dir); err != nil {
					fail(err)
					return
				}
				it.Status = "recloned"
			case r.Pinned:
				it.Status = "pinned"
			default:
				if err := gitops.Update(dir, r.Ref); err != nil {
					fail(err)
					return
				}
			}
			sha, err := gitops.HeadSHA(dir)
			if err != nil {
				fail(err)
				return
			}
			ents, err := gitops.LsFiles(dir)
			if err != nil {
				fail(err)
				return
			}
			st, err := db.IndexRepo(n, dir, ents)
			if err != nil {
				fail(err)
				return
			}
			it.NewSHA, it.Added, it.Changed, it.Removed = sha, st.Added, st.Changed, st.Removed
			if it.Status == "" {
				if sha == r.SHA {
					it.Status = "current"
				} else {
					it.Status = "updated"
				}
			}
			r.SHA = sha
			r.UpdatedAt = time.Now().UTC().Truncate(time.Second)
			it.Seconds = round1(time.Since(t0).Seconds())
		}()
	}
	wg.Wait()
	return res, a.M.Save()
}

// ---- reindex ----

func (a *App) Reindex(names []string) (*UpdateResult, error) {
	if len(names) == 0 {
		names = a.M.Names()
		// A full reindex also drops rows for repos no longer in the manifest.
		if db, err := a.DB(); err == nil {
			if stats, err := db.RepoStats(); err == nil {
				for n := range stats {
					if a.M.Get(n) == nil {
						db.DeleteRepo(n)
					}
				}
			}
		}
	} else {
		var err error
		if names, err = a.ResolveRepos(names); err != nil {
			return nil, err
		}
	}
	db, err := a.DB()
	if err != nil {
		return nil, err
	}
	res := &UpdateResult{}
	for _, n := range names {
		t0 := time.Now()
		it := &UpdateItem{Name: n, Status: "reindexed"}
		res.Items = append(res.Items, it)
		dir := a.dir(n)
		ents, err := gitops.LsFiles(dir)
		if err == nil {
			err = db.DeleteRepo(n)
		}
		var st index.Stats
		if err == nil {
			st, err = db.IndexRepo(n, dir, ents)
		}
		if err != nil {
			it.Status, it.Error = "error", xerr.As(err)
			continue
		}
		sha, _ := gitops.HeadSHA(dir)
		it.NewSHA, it.Added = sha, st.Added
		if r := a.M.Get(n); r != nil && sha != "" {
			r.SHA = sha
		}
		it.Seconds = round1(time.Since(t0).Seconds())
	}
	return res, a.M.Save()
}

// ---- remove ----

type RemoveResult struct {
	Removed []string `json:"removed"`
}

func (a *App) Remove(names []string) (*RemoveResult, error) {
	if len(names) == 0 {
		return nil, xerr.New(xerr.User, "corpus remove <name>", "no repo given")
	}
	// Removal requires exact names: no fuzzy matching for destructive operations.
	for _, n := range names {
		if a.M.Get(n) == nil {
			_, err := a.M.Resolve(n)
			if err == nil {
				return nil, xerr.New(xerr.NotFound, "use the exact name from corpus list", "remove needs an exact repo name, got %q", n)
			}
			return nil, err
		}
	}
	db, err := a.DB()
	if err != nil {
		return nil, err
	}
	res := &RemoveResult{}
	for _, n := range names {
		if err := db.DeleteRepo(n); err != nil {
			return nil, err
		}
		if err := removeAll(a.dir(n)); err != nil {
			return nil, xerr.New(xerr.User, "close programs using the folder and delete it manually", "removed from index but could not delete %s: %v", a.dir(n), err)
		}
		a.M.Remove(n)
		res.Removed = append(res.Removed, n)
	}
	return res, a.M.Save()
}

// removeAll deletes a tree, clearing read-only bits (git packs on Windows).
func removeAll(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			os.Chmod(p, 0o777)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// ---- list ----

type ListItem struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Ref       string    `json:"ref"`
	Pinned    bool      `json:"pinned"`
	SHA       string    `json:"sha"`
	UpdatedAt time.Time `json:"updated_at"`
	Files     int       `json:"files"`
	Chunks    int       `json:"chunks"`
	Langs     []string  `json:"langs"`
}

type ListResult struct {
	Root  string      `json:"root"`
	Repos []*ListItem `json:"repos"`
}

func (a *App) List() (*ListResult, error) {
	db, err := a.DB()
	if err != nil {
		return nil, err
	}
	stats, err := db.RepoStats()
	if err != nil {
		return nil, err
	}
	res := &ListResult{Root: a.RefDir, Repos: []*ListItem{}}
	for _, n := range a.M.Names() {
		r := a.M.Get(n)
		it := &ListItem{Name: r.Name, URL: r.URL, Ref: r.Ref, Pinned: r.Pinned, SHA: r.SHA, UpdatedAt: r.UpdatedAt, Langs: []string{}}
		if s := stats[n]; s != nil {
			it.Files, it.Chunks, it.Langs = s.Files, s.Chunks, s.Langs
		}
		res.Repos = append(res.Repos, it)
	}
	return res, nil
}

// ---- status ----

type Issue struct {
	Repo    string `json:"repo,omitempty"`
	Problem string `json:"problem"`
	Fix     string `json:"fix"`
}

type StatusResult struct {
	Root     string   `json:"root"`
	Repos    int      `json:"repos"`
	Git      string   `json:"git"`
	Ripgrep  string   `json:"ripgrep"`
	Reranker string   `json:"reranker"`
	Issues   []*Issue `json:"issues"`
}

func (a *App) Status() (*StatusResult, error) {
	res := &StatusResult{Root: a.RefDir, Repos: len(a.M.Repos), Issues: []*Issue{}}
	res.Git = "ok"
	if err := gitops.Available(); err != nil {
		res.Git = "missing"
		res.Issues = append(res.Issues, &Issue{Problem: "git not on PATH", Fix: "install git"})
	}
	res.Ripgrep = "ok"
	if _, err := exec.LookPath("rg"); err != nil {
		res.Ripgrep = "missing (using slower git grep)"
	}
	if rc, why := rerank.FromEnv(); rc != nil {
		res.Reranker = rc.Describe()
	} else {
		res.Reranker = "off: " + why
	}
	db, err := a.DB()
	if err != nil {
		return nil, err
	}
	stats, err := db.RepoStats()
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, n := range a.M.Names() {
		tracked[n] = true
		r := a.M.Get(n)
		dir := a.dir(n)
		if !isDir(filepath.Join(dir, ".git")) {
			res.Issues = append(res.Issues, &Issue{Repo: n, Problem: "checkout missing", Fix: "corpus update " + n + "  (re-clones)"})
			continue
		}
		if sha, err := gitops.HeadSHA(dir); err == nil && sha != r.SHA {
			res.Issues = append(res.Issues, &Issue{Repo: n, Problem: "checkout differs from manifest", Fix: "corpus reindex " + n})
		}
		if s := stats[n]; s == nil || s.Files == 0 {
			res.Issues = append(res.Issues, &Issue{Repo: n, Problem: "not indexed", Fix: "corpus reindex " + n})
		}
	}
	for n := range stats {
		if !tracked[n] {
			res.Issues = append(res.Issues, &Issue{Repo: n, Problem: "index rows for untracked repo", Fix: "corpus reindex  (or re-add it)"})
		}
	}
	ents, _ := os.ReadDir(a.RefDir)
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !tracked[e.Name()] {
			res.Issues = append(res.Issues, &Issue{Repo: e.Name(), Problem: "untracked directory in references/",
				Fix: "delete " + a.dir(e.Name()) + " or add it with corpus add <url> --name " + e.Name() + " after deleting"})
		}
	}
	return res, nil
}

func (r *StatusResult) ExitCode() int {
	if len(r.Issues) > 0 {
		return xerr.Partial
	}
	return xerr.OK
}
