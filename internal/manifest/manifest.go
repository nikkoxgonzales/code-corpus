// Package manifest stores the list of onboarded repos in references/.corpus/corpus.json.
package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

const SchemaVersion = 1

type Repo struct {
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	Ref        string    `json:"ref"`    // branch, tag or commit being tracked
	Pinned     bool      `json:"pinned"` // tag or commit: update is a no-op
	SHA        string    `json:"sha"`
	Categories []string  `json:"categories,omitempty"`
	AddedAt    time.Time `json:"added_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Manifest struct {
	SchemaVersion int     `json:"schema_version"`
	Repos         []*Repo `json:"repos"`

	path string
	mu   sync.Mutex
}

func Load(path string) (*Manifest, error) {
	m := &Manifest{SchemaVersion: SchemaVersion, path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, xerr.New(xerr.User, "fix or delete "+path+", then run: corpus status", "corrupt manifest: %v", err)
	}
	return m, nil
}

// Save writes atomically (temp file + rename).
func (m *Manifest) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sort.Slice(m.Repos, func(i, j int) bool { return m.Repos[i].Name < m.Repos[j].Name })
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func (m *Manifest) Get(name string) *Repo {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.Repos {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func (m *Manifest) ByURL(url string) *Repo {
	m.mu.Lock()
	defer m.mu.Unlock()
	norm := normURL(url)
	for _, r := range m.Repos {
		if normURL(r.URL) == norm {
			return r
		}
	}
	return nil
}

func (m *Manifest) Put(r *Repo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, x := range m.Repos {
		if x.Name == r.Name {
			m.Repos[i] = r
			return
		}
	}
	m.Repos = append(m.Repos, r)
}

func (m *Manifest) Remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.Repos[:0]
	for _, r := range m.Repos {
		if r.Name != name {
			out = append(out, r)
		}
	}
	m.Repos = out
}

func (m *Manifest) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.Repos {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// Resolve finds a repo by exact name, case-insensitive name, or unique substring.
func (m *Manifest) Resolve(q string) (*Repo, error) {
	if r := m.Get(q); r != nil {
		return r, nil
	}
	lq := strings.ToLower(q)
	var subs []*Repo
	for _, r := range m.Repos {
		ln := strings.ToLower(r.Name)
		if ln == lq {
			return r, nil
		}
		if strings.Contains(ln, lq) || strings.Contains(lq, ln) {
			subs = append(subs, r)
		}
	}
	if len(subs) == 1 {
		return subs[0], nil
	}
	if len(m.Repos) == 0 {
		return nil, xerr.New(xerr.NotFound, "corpus add <owner/repo>", "repo %q not found: corpus is empty", q)
	}
	var names []string
	for _, r := range subs {
		names = append(names, r.Name)
	}
	if len(names) > 0 {
		return nil, xerr.New(xerr.NotFound, "use one of: "+strings.Join(names, ", "), "repo %q is ambiguous", q)
	}
	return nil, xerr.New(xerr.NotFound, "corpus list  (or corpus add <owner/repo>)", "repo %q not found", q)
}

// Tag adds (or with remove, drops) categories on a repo. Returns false if the repo is unknown.
func (m *Manifest) Tag(name string, cats []string, remove bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.Repos {
		if r.Name != name {
			continue
		}
		set := map[string]bool{}
		for _, c := range r.Categories {
			set[c] = true
		}
		for _, c := range cats {
			set[c] = !remove
		}
		r.Categories = r.Categories[:0]
		for c, on := range set {
			if on {
				r.Categories = append(r.Categories, c)
			}
		}
		sort.Strings(r.Categories)
		return true
	}
	return false
}

// InCategory returns repo names carrying category cat.
func (m *Manifest) InCategory(cat string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.Repos {
		for _, c := range r.Categories {
			if c == cat {
				out = append(out, r.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Categories returns every category with its repo count.
func (m *Manifest) Categories() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, r := range m.Repos {
		for _, c := range r.Categories {
			out[c]++
		}
	}
	return out
}

func normURL(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "git@")
	return strings.Replace(u, ":", "/", 1)
}
