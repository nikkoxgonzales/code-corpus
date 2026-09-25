package app

import (
	"testing"

	"github.com/nikkoxgonzales/code-corpus/internal/manifest"
	"github.com/nikkoxgonzales/code-corpus/internal/search"
)

func TestParseSource(t *testing.T) {
	cases := []struct{ in, url, name, ref string }{
		{"grpc/grpc-go", "https://github.com/grpc/grpc-go", "grpc-go", ""},
		{"psf/requests@v2.32.0", "https://github.com/psf/requests", "requests", "v2.32.0"},
		{"https://github.com/BurntSushi/ripgrep.git", "https://github.com/BurntSushi/ripgrep", "ripgrep", ""},
		{"https://github.com/expressjs/express/tree/5.x", "https://github.com/expressjs/express", "express", "5.x"},
		{"git@github.com:sindresorhus/ky.git", "git@github.com:sindresorhus/ky", "ky", ""},
		{"github.com/a/b@main", "https://github.com/a/b", "b", "main"},
	}
	for _, c := range cases {
		url, _, name, ref, err := ParseSource(c.in)
		if err != nil || url != c.url || name != c.name || ref != c.ref {
			t.Errorf("ParseSource(%q) = %q %q %q %v; want %q %q %q", c.in, url, name, ref, err, c.url, c.name, c.ref)
		}
	}
	if _, _, _, _, err := ParseSource("not a repo"); err == nil {
		t.Error("expected error for garbage source")
	}
}

func TestParseTarget(t *testing.T) {
	a := &App{RefDir: `F:\code-corpus\references`, M: &manifest.Manifest{Repos: []*manifest.Repo{{Name: "grpc-go"}}}}
	cases := []struct {
		in         string
		path       string
		start, end int
	}{
		{"grpc-go:server.go:10-20", "server.go", 10, 20},
		{"grpc-go:internal/backoff/backoff.go:41", "internal/backoff/backoff.go", 41, 41},
		{"grpc-go:clientconn.go", "clientconn.go", 0, 0},
		{"grpc-go/server.go", "server.go", 0, 0},
		{`F:\code-corpus\references\grpc-go\server.go:5`, "server.go", 5, 5},
		{"GRPC-GO:server.go", "server.go", 0, 0},
	}
	for _, c := range cases {
		repo, p, s, e, err := a.ParseTarget(c.in)
		if err != nil || repo != "grpc-go" || p != c.path || s != c.start || e != c.end {
			t.Errorf("ParseTarget(%q) = %q %q %d %d %v", c.in, repo, p, s, e, err)
		}
	}
}

func TestDetectMode(t *testing.T) {
	for q, want := range map[string]string{
		"parseConfig":                 search.Symbol,
		"http.Client":                 search.Symbol,
		"Foo::bar":                    search.Symbol,
		`"retry budget"`:              search.Exact,
		`func \w+Handler`:             search.Regex,
		"how are retries backed off?": search.Concept,
	} {
		if got := search.DetectMode(q); got != want {
			t.Errorf("DetectMode(%q) = %s, want %s", q, got, want)
		}
	}
}
