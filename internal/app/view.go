package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nikkoxgonzales/code-corpus/internal/chunk"
	"github.com/nikkoxgonzales/code-corpus/internal/gitops"
	"github.com/nikkoxgonzales/code-corpus/internal/index"
	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

const maxShow = 400

type ShowResult struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Lang    string `json:"lang,omitempty"`
	Start   int    `json:"start_line"`
	End     int    `json:"end_line"`
	Total   int    `json:"total_lines"`
	Content string `json:"content"`
	Next    string `json:"next,omitempty"`
}

var rangeRe = regexp.MustCompile(`^(.*?):(\d+)(?:-(\d+))?$`)

// ParseTarget splits "repo:path[:start[-end]]" (or "repo/path") into parts.
func (a *App) ParseTarget(t string) (repo, p string, start, end int, err error) {
	t = strings.TrimSpace(strings.ReplaceAll(t, `\`, "/"))
	ref := filepath.ToSlash(a.RefDir) + "/"
	if i := strings.Index(strings.ToLower(t), strings.ToLower(ref)); i >= 0 {
		t = t[i+len(ref):]
	}
	t = strings.TrimPrefix(t, "references/")
	r, rest, ok := strings.Cut(t, ":")
	if !ok || strings.Contains(r, "/") {
		r, rest, ok = strings.Cut(t, "/")
		if !ok {
			return "", "", 0, 0, xerr.New(xerr.User, "format: <repo>:<path>[:<start>[-<end>]]", "cannot parse target %q", t)
		}
	}
	rr, err := a.M.Resolve(r)
	if err != nil {
		return "", "", 0, 0, err
	}
	if m := rangeRe.FindStringSubmatch(rest); m != nil {
		rest = m[1]
		start, _ = strconv.Atoi(m[2])
		end = start
		if m[3] != "" {
			end, _ = strconv.Atoi(m[3])
		}
	}
	return rr.Name, strings.Trim(rest, "/"), start, end, nil
}

func (a *App) Show(target string, context int) (*ShowResult, error) {
	repo, p, start, end, err := a.ParseTarget(target)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(a.dir(repo), filepath.FromSlash(p))
	st, err := os.Stat(full)
	if err != nil {
		return nil, xerr.New(xerr.NotFound, fmt.Sprintf("corpus tree %s %s", repo, filepath.ToSlash(filepath.Dir(p))), "file %s:%s not found", repo, p)
	}
	if st.IsDir() {
		return nil, xerr.New(xerr.User, fmt.Sprintf("corpus tree %s %s", repo, p), "%s:%s is a directory", repo, p)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	lines := chunk.SplitLines(string(b))
	res := &ShowResult{Repo: repo, Path: p, Lang: chunk.Lang(p), Total: len(lines)}
	if start == 0 {
		start, end = 1, min(len(lines), 200)
	}
	if end < start {
		end = start
	}
	start, end = max(1, start-context), min(len(lines), end+context)
	if end-start+1 > maxShow {
		end = start + maxShow - 1
	}
	res.Start, res.End = start, end
	if start > len(lines) {
		return nil, xerr.New(xerr.User, fmt.Sprintf("file has %d lines", len(lines)), "line %d is past the end of %s:%s", start, repo, p)
	}
	var sb strings.Builder
	w := len(strconv.Itoa(end))
	for n := start; n <= end; n++ {
		fmt.Fprintf(&sb, "%*d| %s\n", w, n, strings.TrimRight(lines[n-1], " \t"))
	}
	res.Content = sb.String()
	if end < len(lines) {
		res.Next = fmt.Sprintf("corpus show %s:%s:%d-%d", repo, p, end+1, min(len(lines), end+200))
	}
	return res, nil
}

func (r *ShowResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s:%s:%d-%d (of %d lines", r.Repo, r.Path, r.Start, r.End, r.Total)
	if r.Lang != "" {
		b.WriteString(", " + r.Lang)
	}
	b.WriteString(")\n")
	b.WriteString(r.Content)
	if r.Next != "" {
		b.WriteString("# next: " + r.Next + "\n")
	}
	return b.String()
}

// ---- tree ----

type TreeEntry struct {
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Files int    `json:"files,omitempty"` // files beneath, for directories
	Depth int    `json:"depth"`
	Tag   string `json:"tag,omitempty"` // test | vendor | doc | generated
}

type TreeResult struct {
	Repo    string       `json:"repo"`
	Path    string       `json:"path"`
	Files   int          `json:"files"`
	Depth   int          `json:"depth"`
	Entries []*TreeEntry `json:"entries"`
	Elided  int          `json:"elided"`
	Next    string       `json:"next,omitempty"`
}

type node struct {
	name  string
	kids  map[string]*node
	files int
	leaf  bool
}

func (a *App) Tree(repoName, sub string, depth int) (*TreeResult, error) {
	r, err := a.M.Resolve(repoName)
	if err != nil {
		return nil, err
	}
	if depth <= 0 {
		depth = 2
	}
	sub = strings.Trim(strings.ReplaceAll(sub, `\`, "/"), "/")
	ents, err := gitops.LsFiles(a.dir(r.Name))
	if err != nil {
		return nil, err
	}
	root := &node{kids: map[string]*node{}}
	total := 0
	for _, e := range ents {
		p := e.Path
		if sub != "" {
			if !strings.HasPrefix(p, sub+"/") {
				continue
			}
			p = p[len(sub)+1:]
		}
		total++
		cur := root
		cur.files++
		segs := strings.Split(p, "/")
		for i, s := range segs {
			k := cur.kids[s]
			if k == nil {
				k = &node{name: s, kids: map[string]*node{}, leaf: i == len(segs)-1}
				cur.kids[s] = k
			}
			k.files++
			cur = k
		}
	}
	if total == 0 {
		return nil, xerr.New(xerr.NotFound, "corpus tree "+r.Name, "no files under %s:%s", r.Name, sub)
	}
	res := &TreeResult{Repo: r.Name, Path: sub, Files: total, Depth: depth, Entries: []*TreeEntry{}}
	const perDir, maxEntries = 25, 300
	var walk func(n *node, prefix string, d int)
	walk = func(n *node, prefix string, d int) {
		var dirs, files []*node
		for _, k := range n.kids {
			if k.leaf {
				files = append(files, k)
			} else {
				dirs = append(dirs, k)
			}
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].name < dirs[j].name })
		sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
		for _, k := range dirs {
			if len(res.Entries) >= maxEntries {
				res.Elided += k.files
				continue
			}
			p := prefix + k.name
			res.Entries = append(res.Entries, &TreeEntry{Path: p + "/", Dir: true, Files: k.files, Depth: d, Tag: tagOf(p + "/x")})
			if d < depth {
				walk(k, p+"/", d+1)
			}
		}
		for i, k := range files {
			if i >= perDir || len(res.Entries) >= maxEntries {
				res.Elided += len(files) - i
				res.Entries = append(res.Entries, &TreeEntry{Path: fmt.Sprintf("%s... +%d files", prefix, len(files)-i), Depth: d})
				break
			}
			res.Entries = append(res.Entries, &TreeEntry{Path: prefix + k.name, Depth: d})
		}
	}
	walk(root, "", 1)
	// Suggest drilling into the largest directory.
	var big *TreeEntry
	for _, e := range res.Entries {
		if e.Dir && e.Depth == 1 && e.Tag == "" && (big == nil || e.Files > big.Files) {
			big = e
		}
	}
	if big != nil {
		p := strings.TrimSuffix(big.Path, "/")
		if sub != "" {
			p = sub + "/" + p
		}
		res.Next = fmt.Sprintf("corpus tree %s %s --depth %d", r.Name, p, depth)
	}
	return res, nil
}

func tagOf(p string) string {
	f := index.PathFlags(p)
	switch {
	case f&index.FlagVendor != 0:
		return "vendor"
	case f&index.FlagTest != 0:
		return "test"
	case f&index.FlagGenerated != 0:
		return "generated"
	case f&index.FlagDoc != 0 && chunk.Lang(p) == "":
		return "doc"
	}
	return ""
}

func (r *TreeResult) Text() string {
	var b strings.Builder
	loc := r.Repo
	if r.Path != "" {
		loc += ":" + r.Path
	}
	fmt.Fprintf(&b, "# %s/ (%d files, depth %d)\n", loc, r.Files, r.Depth)
	for _, e := range r.Entries {
		ind := strings.Repeat("  ", e.Depth-1)
		name := e.Path
		if i := strings.LastIndex(strings.TrimSuffix(name, "/"), "/"); i >= 0 && !strings.Contains(name, "... +") {
			name = name[i+1:]
		} else if strings.Contains(name, "... +") {
			name = name[strings.Index(name, "... +"):]
		}
		b.WriteString(ind + name)
		if e.Dir {
			fmt.Fprintf(&b, " (%d)", e.Files)
		}
		if e.Tag != "" {
			b.WriteString(" [" + e.Tag + "]")
		}
		b.WriteByte('\n')
	}
	if r.Next != "" {
		b.WriteString("# next: " + r.Next + "\n")
	}
	return b.String()
}
