package chunk

import (
	"path"
	"strings"
)

// Block styles: how the end of a definition is found.
const (
	styleBrace   = iota // count { }
	styleIndent         // python: dedent ends the block
	styleEnd            // ruby/lua/elixir: matching "end" at the same indent
	styleHeading        // markdown: next heading
	styleWindow         // fixed-size windows only
)

type langSpec struct {
	style        int
	singleQuotes bool // ' delimits strings (false for rust lifetimes)
	defs         []def
}

var extLang = map[string]string{
	".go": "go", ".py": "python", ".pyi": "python",
	".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".ts": "typescript", ".tsx": "typescript", ".mts": "typescript", ".cts": "typescript",
	".vue": "typescript", ".svelte": "typescript",
	".rs": "rust", ".java": "java", ".kt": "kotlin", ".kts": "kotlin", ".scala": "scala",
	".cs": "csharp", ".dart": "dart", ".groovy": "java",
	".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp", ".hxx": "cpp",
	".m": "c", ".mm": "cpp", ".swift": "swift", ".rb": "ruby", ".php": "php", ".lua": "lua",
	".ex": "elixir", ".exs": "elixir", ".zig": "zig", ".proto": "proto",
	".sh": "shell", ".bash": "shell", ".zsh": "shell", ".ps1": "powershell", ".psm1": "powershell",
	".sql": "sql", ".hs": "haskell", ".ml": "ocaml", ".clj": "clojure", ".erl": "erlang", ".jl": "julia", ".r": "r",
	".md": "markdown", ".mdx": "markdown", ".rst": "text", ".txt": "text", ".adoc": "text",
	".json": "config", ".yaml": "config", ".yml": "config", ".toml": "config", ".ini": "config",
	".cfg": "config", ".gradle": "config", ".xml": "config", ".tf": "config", ".nix": "config",
	".html": "html", ".css": "css", ".scss": "css", ".graphql": "config", ".gql": "config",
}

var nameLang = map[string]string{
	"makefile": "config", "dockerfile": "config", "cmakelists.txt": "config", "justfile": "config",
	"gemfile": "ruby", "rakefile": "ruby", "build": "config", "workspace": "config",
}

// langExts is the inverse map, used to turn --lang into file globs.
var langExts = map[string][]string{}

func init() {
	for e, l := range extLang {
		langExts[l] = append(langExts[l], e)
	}
}

// Lang returns the language of a path, or "" when the file should not be indexed.
func Lang(p string) string {
	base := strings.ToLower(path.Base(p))
	if l, ok := nameLang[base]; ok {
		return l
	}
	return extLang[path.Ext(base)]
}

// Exts returns the file extensions for a language name (for ripgrep globs).
func Exts(lang string) []string { return langExts[strings.ToLower(lang)] }

// KnownLang reports whether lang is a language name corpus understands.
func KnownLang(lang string) bool { _, ok := langExts[strings.ToLower(lang)]; return ok }

// Langs lists all language names.
func Langs() []string {
	var out []string
	for l := range langExts {
		out = append(out, l)
	}
	return out
}
