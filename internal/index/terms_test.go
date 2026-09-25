package index

import (
	"slices"
	"strings"
	"testing"
)

func TestParts(t *testing.T) {
	for in, want := range map[string]string{
		"parseHTTPConfig": "parse http config",
		"max_retries":     "max retries",
		"XMLHttpRequest":  "xml http request",
		"utf8Decode":      "utf8 decode",
	} {
		if got := strings.Join(parts(in), " "); got != want {
			t.Errorf("parts(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandIncludesFullAndParts(t *testing.T) {
	got := strings.Fields(Expand("x := parseConfig(y)"))
	for _, w := range []string{"parseconfig", "parse", "config", "x", "y"} {
		if !slices.Contains(got, w) {
			t.Errorf("Expand missing %q: %v", w, got)
		}
	}
}

func TestQueryTerms(t *testing.T) {
	got := QueryTerms("how are connection retries backed off")
	for _, w := range []string{"connection", "retries", "backoff", "conn"} {
		if !slices.Contains(got, w) {
			t.Errorf("QueryTerms missing %q: %v", w, got)
		}
	}
	for _, w := range []string{"how", "are"} {
		if slices.Contains(got, w) {
			t.Errorf("QueryTerms kept stopword %q", w)
		}
	}
	if got := QueryTerms("maximum receive message size"); !slices.Contains(got, "recv") || !slices.Contains(got, "msg") {
		t.Errorf("abbreviations not expanded: %v", got)
	}
	if got := QueryTerms("user log in"); !slices.Contains(got, "login") {
		t.Errorf("particle compound missing: %v", got)
	}
}

func TestGlobPattern(t *testing.T) {
	for in, want := range map[string]string{"internal/": "*internal/*", "src/**/*.ts": "src/*/*.ts", `a\b*`: "a/b*"} {
		if got := GlobPattern(in); got != want {
			t.Errorf("GlobPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathFlags(t *testing.T) {
	cases := map[string]int{
		"pkg/foo_test.go":       FlagTest,
		"vendor/x/y.go":         FlagVendor,
		"api/service.pb.go":     FlagGenerated,
		"docs/guide.md":         FlagDoc,
		"src/main.go":           0,
		"src/FooTest.java":      FlagTest,
		"web/app.spec.ts":       FlagTest,
		"third_party/lib/a.cpp": FlagVendor,
	}
	for p, want := range cases {
		if got := PathFlags(p); got != want {
			t.Errorf("PathFlags(%q) = %d, want %d", p, got, want)
		}
	}
}
