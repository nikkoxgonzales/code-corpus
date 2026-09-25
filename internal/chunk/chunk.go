// Package chunk splits source files into definition-level chunks and extracts
// symbols, using per-language regexes and block-end heuristics (no cgo, no
// tree-sitter). Chunks never overlap and together cover the non-trivial lines.
package chunk

import (
	"regexp"
	"strings"
)

const (
	maxChunk = 150 // defs longer than this are split into header + nested chunks/windows
	window   = 80  // lines per window for code outside definitions
	minDef   = 3   // shorter defs stay in the surrounding window (still recorded as symbols)
)

type Chunk struct {
	Start, End int // 1-based, inclusive
	Symbol     string
	Kind       string
}

type Symbol struct {
	Name string
	Kind string
	Line int // 1-based
}

type Result struct {
	Chunks  []Chunk
	Symbols []Symbol
}

type def struct {
	re   *regexp.Regexp
	kind string
}

type found struct {
	line, end int // 0-based
	name      string
	kind      string
}

func d(kind, pattern string) def { return def{regexp.MustCompile(pattern), kind} }

var jvmMods = `(?:(?:public|private|protected|internal|static|final|abstract|sealed|partial|open|data|inner|override|virtual|readonly|unsafe|export|synchronized|native|async|extern|new|default|suspend|inline|operator|infix|tailrec|const)\s+)`

var specs = map[string]langSpec{
	"go": {style: styleBrace, singleQuotes: true, defs: []def{
		d("method", `^func\s+\([^)]*\)\s*(?P<n>[A-Za-z_]\w*)`),
		d("func", `^func\s+(?P<n>[A-Za-z_]\w*)`),
		d("type", `^type\s+(?P<n>[A-Za-z_]\w*)`),
		d("var", `^(?:var|const)\s+(?P<n>[A-Za-z_]\w*)\b`),
	}},
	"python": {style: styleIndent, defs: []def{
		d("func", `^\s*(?:async\s+)?def\s+(?P<n>[A-Za-z_]\w*)`),
		d("class", `^\s*class\s+(?P<n>[A-Za-z_]\w*)`),
	}},
	"javascript": {style: styleBrace, singleQuotes: true, defs: jsDefs},
	"typescript": {style: styleBrace, singleQuotes: true, defs: jsDefs},
	"rust": {style: styleBrace, defs: []def{
		d("func", `^\s*(?:pub(?:\([^)]*\))?\s+)?(?:default\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+(?P<n>\w+)`),
		d("type", `^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|union|trait|type)\s+(?P<n>\w+)`),
		d("impl", `^\s*(?:unsafe\s+)?impl(?:<[^>]*>)?\s+(?:!?[\w:]+(?:<[^>]*>)?\s+for\s+)?(?P<n>[\w:]+)`),
		d("module", `^\s*(?:pub(?:\([^)]*\))?\s+)?mod\s+(?P<n>\w+)\s*\{`),
		d("macro", `^\s*macro_rules!\s*(?P<n>\w+)`),
	}},
	"java":   {style: styleBrace, singleQuotes: true, defs: jvmDefs},
	"csharp": {style: styleBrace, singleQuotes: true, defs: jvmDefs},
	"dart":   {style: styleBrace, singleQuotes: true, defs: jvmDefs},
	"kotlin": {style: styleBrace, defs: append([]def{
		d("func", `^\s*`+jvmMods+`*fun\s+(?:<[^>]*>\s*)?(?:[\w.]+\.)?(?P<n>\w+)`),
	}, jvmDefs...)},
	"scala": {style: styleBrace, defs: append([]def{
		d("func", `^\s*(?:(?:override|private|protected|final|implicit|inline)\s+)*def\s+(?P<n>\w+)`),
	}, jvmDefs...)},
	"c":   {style: styleBrace, singleQuotes: true, defs: cDefs},
	"cpp": {style: styleBrace, singleQuotes: true, defs: cDefs},
	"swift": {style: styleBrace, singleQuotes: true, defs: []def{
		d("func", `^\s*(?:(?:public|private|fileprivate|internal|open|static|class|final|override|mutating|nonisolated|@\w+)\s+)*func\s+(?P<n>\w+)`),
		d("class", `^\s*(?:(?:public|private|fileprivate|internal|open|final|@\w+)\s+)*(?:class|struct|enum|protocol|extension|actor)\s+(?P<n>\w+)`),
	}},
	"php": {style: styleBrace, singleQuotes: true, defs: []def{
		d("func", `^\s*(?:(?:public|private|protected|static|abstract|final)\s+)*function\s+&?(?P<n>\w+)`),
		d("class", `^\s*(?:(?:abstract|final|readonly)\s+)*(?:class|interface|trait|enum)\s+(?P<n>\w+)`),
	}},
	"ruby": {style: styleEnd, singleQuotes: true, defs: []def{
		d("func", `^\s*def\s+(?:self\.)?(?P<n>[\w?!=\[\]<>+\-*/%]+)`),
		d("class", `^\s*(?:class|module)\s+(?P<n>[\w:]+)`),
	}},
	"lua": {style: styleEnd, singleQuotes: true, defs: []def{
		d("func", `^\s*(?:local\s+)?function\s+(?P<n>[\w.:]+)`),
		d("func", `^\s*(?:local\s+)?(?P<n>[\w.]+)\s*=\s*function\b`),
	}},
	"elixir": {style: styleEnd, defs: []def{
		d("func", `^\s*(?:def|defp|defmacro|defmacrop|defguard)\s+(?P<n>\w+[?!]?)`),
		d("module", `^\s*defmodule\s+(?P<n>[\w.]+)`),
	}},
	"zig": {style: styleBrace, defs: []def{
		d("func", `^\s*(?:pub\s+)?(?:export\s+)?(?:inline\s+)?fn\s+(?P<n>\w+)`),
		d("type", `^\s*(?:pub\s+)?const\s+(?P<n>\w+)\s*=\s*(?:packed\s+|extern\s+)?(?:struct|enum|union)`),
	}},
	"proto": {style: styleBrace, defs: []def{
		d("type", `^\s*(?:message|service|enum)\s+(?P<n>\w+)`),
		d("func", `^\s*rpc\s+(?P<n>\w+)`),
	}},
	"shell": {style: styleBrace, defs: []def{
		d("func", `^\s*function\s+(?P<n>[\w.:-]+)`),
		d("func", `^\s*(?P<n>[\w.:-]+)\s*\(\)\s*\{?\s*$`),
	}},
	"powershell": {style: styleBrace, defs: []def{
		d("func", `(?i)^\s*function\s+(?P<n>[\w-]+)`),
		d("class", `(?i)^\s*class\s+(?P<n>\w+)`),
	}},
	"sql": {style: styleWindow, defs: []def{
		d("type", `(?i)^\s*create\s+(?:or\s+replace\s+)?(?:table|view|function|procedure|index|trigger|type)\s+(?:if\s+not\s+exists\s+)?(?P<n>[\w."]+)`),
	}},
	"markdown": {style: styleHeading},
}

var jsDefs = []def{
	d("func", `^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:async\s+)?function\s*\*?\s*(?P<n>[\w$]+)`),
	d("class", `^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?class\s+(?P<n>[\w$]+)`),
	d("type", `^\s*(?:export\s+)?(?:declare\s+)?(?:interface|enum)\s+(?P<n>[\w$]+)`),
	d("type", `^\s*(?:export\s+)?(?:declare\s+)?type\s+(?P<n>[\w$]+)\s*(?:<[^=]*>)?\s*=`),
	d("func", `^\s*(?:export\s+)?(?:const|let|var)\s+(?P<n>[\w$]+)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*(?::[^=]+)?=>|[\w$]+\s*=>|\(\s*$)`),
	// res.sendFile = function sendFile(...)  /  Foo.prototype.bar = function  /  exports.x = async (...) =>
	d("method", `^\s*(?:[\w$]+\.)+(?P<n>[\w$]+)\s*=\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*=>|[\w$]+\s*=>)`),
	d("method", `^\s+(?:(?:public|private|protected|static|async|readonly|override|abstract|get|set)\s+)*\*?(?P<n>[\w$]+)\s*(?:<[^>]*>)?\s*\([^()]*\)\s*(?::\s*[^{()]+)?\{\s*$`),
}

var jvmDefs = []def{
	d("class", `^\s*`+jvmMods+`*(?:class|interface|enum|record|struct|object|trait|mixin)\s+(?P<n>\w+)`),
	d("method", `^\s*`+jvmMods+`+(?:<[^>]*>\s*)?[\w<>\[\],.?]+(?:\s*<[^>]*>)?\s+(?P<n>\w+)\s*\(`),
	d("method", `^\s+[\w<>\[\],.?]+\s+(?P<n>\w+)\s*\([^;]*\)\s*(?:throws\s+[\w.,\s]+)?\{?\s*$`),
}

var cDefs = []def{
	d("type", `^(?:typedef\s+)?(?:struct|union|enum|class)\s+(?P<n>\w+)[^;]*$`),
	d("module", `^namespace\s+(?P<n>[\w:]+)\s*\{`),
	d("macro", `^#\s*define\s+(?P<n>\w+)`),
	d("func", `^(?:template\s*<[^>]*>\s*)?(?:(?:static|inline|extern|virtual|constexpr|const|unsigned|signed|struct|enum)\s+)*[A-Za-z_][\w:<>,\s\*&]*?[\s\*&]+\**(?P<n>[A-Za-z_][\w:~]*)\s*\([^;]*$`),
	d("func", `^(?P<n>[A-Za-z_][\w:~]*)\s*\([^;]*$`),
}

var keywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields("if for while switch catch return else new function sizeof elif when with do try using lock foreach synchronized throw await yield typeof delete void case super this constructor") {
		keywords[k] = true
	}
}

// SplitLines splits text into lines, dropping \r.
func SplitLines(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(src, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Split chunks the given source. lang comes from Lang().
func Split(lang, src string) Result {
	lines := SplitLines(src)
	spec, ok := specs[lang]
	if !ok {
		spec = langSpec{style: styleWindow}
	}
	var defs []found
	if spec.style == styleHeading {
		defs = headings(lines)
	} else {
		defs = findDefs(lines, spec)
	}

	var res Result
	var cdefs []found
	for _, f := range defs {
		res.Symbols = append(res.Symbols, Symbol{Name: f.name, Kind: f.kind, Line: f.line + 1})
		if f.end-f.line+1 >= minDef && spec.style != styleWindow {
			cdefs = append(cdefs, f)
		}
	}
	e := emitter{lines: lines}
	e.emit(0, len(lines)-1, cdefs, "")
	res.Chunks = e.out
	return res
}

type emitter struct {
	lines []string
	out   []Chunk
}

func (e *emitter) add(start, end int, sym, kind string) {
	e.out = append(e.out, Chunk{Start: start + 1, End: end + 1, Symbol: sym, Kind: kind})
}

func (e *emitter) emit(lo, hi int, defs []found, parent string) {
	cur := lo
	for i := 0; i < len(defs); {
		f := defs[i]
		if f.line < cur { // overlapped a previous block (bad end estimate)
			i++
			continue
		}
		if f.end > hi {
			f.end = hi
		}
		j := i + 1
		for j < len(defs) && defs[j].line <= f.end {
			j++
		}
		nested := defs[i+1 : j]
		start := leadingComments(e.lines, f.line, cur)
		e.gap(cur, start-1, parent)
		if f.end-start+1 <= maxChunk {
			e.add(start, f.end, f.name, f.kind)
		} else {
			hEnd := start + 30
			if len(nested) > 0 && nested[0].line-1 < hEnd {
				hEnd = max(nested[0].line-1, f.line)
			}
			hEnd = min(hEnd, f.end)
			e.add(start, hEnd, f.name, f.kind)
			e.emit(hEnd+1, f.end, nested, f.name)
		}
		cur = f.end + 1
		i = j
	}
	e.gap(cur, hi, parent)
}

// gap covers lines [a,b] that belong to no definition with fixed windows.
func (e *emitter) gap(a, b int, parent string) {
	for s := a; s <= b; s += window {
		end := min(s+window-1, b)
		if nonTrivial(e.lines[s:end+1]) >= 2 {
			e.add(s, end, parent, "block")
		}
	}
}

func nonTrivial(lines []string) int {
	n := 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || t == "}" || t == "};" || t == ")" || t == "end" || t == "]" || t == "})" || t == "});" {
			continue
		}
		n++
	}
	return n
}

func leadingComments(lines []string, line, floor int) int {
	s := line
	for i := line - 1; i >= floor && line-i <= 30; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			break
		}
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "/*") ||
			strings.HasPrefix(t, "*") || strings.HasPrefix(t, "--") || strings.HasPrefix(t, "@") ||
			strings.HasPrefix(t, "[") || strings.HasPrefix(t, ";;") {
			s = i
			continue
		}
		break
	}
	return s
}

func findDefs(lines []string, spec langSpec) []found {
	var out []found
	for i, l := range lines {
		if len(l) > 400 || strings.TrimSpace(l) == "" {
			continue
		}
		for _, df := range spec.defs {
			m := df.re.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			name := m[df.re.SubexpIndex("n")]
			if name == "" || keywords[name] {
				continue
			}
			kind := df.kind
			if kind == "func" && spec.style == styleIndent && indent(l) > 0 {
				kind = "method"
			}
			f := found{line: i, name: name, kind: kind}
			switch spec.style {
			case styleBrace:
				f.end = braceEnd(lines, i, spec.singleQuotes)
			case styleIndent:
				f.end = indentEnd(lines, i)
			case styleEnd:
				f.end = endEnd(lines, i)
			default:
				f.end = i
			}
			out = append(out, f)
			break
		}
	}
	return out
}

func headings(lines []string) []found {
	var out []found
	inFence := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(l, "#") {
			continue
		}
		name := strings.TrimSpace(strings.TrimLeft(l, "#"))
		if name == "" || !strings.HasPrefix(strings.TrimLeft(l, "#"), " ") {
			continue
		}
		if len(out) > 0 {
			out[len(out)-1].end = i - 1
		}
		out = append(out, found{line: i, end: len(lines) - 1, name: name, kind: "section"})
	}
	return out
}

func indent(l string) int {
	n := 0
	for _, c := range l {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

func countBraces(l string, sq bool) (open, close int) {
	var inStr byte
	for i := 0; i < len(l); i++ {
		c := l[i]
		if inStr != 0 {
			if c == '\\' {
				i++
			} else if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '`':
			inStr = c
		case '\'':
			if sq {
				inStr = c
			}
		case '/':
			if i+1 < len(l) && l[i+1] == '/' {
				return
			}
		case '{':
			open++
		case '}':
			close++
		}
	}
	return
}

func braceEnd(lines []string, start int, sq bool) int {
	depth, opened := 0, false
	for i := start; i < len(lines); i++ {
		o, c := countBraces(lines[i], sq)
		if !opened {
			if o == 0 {
				t := strings.TrimSpace(lines[i])
				if strings.HasSuffix(t, ";") || i-start >= 8 || (t == "" && i > start) {
					return start // declaration without a body
				}
				continue
			}
			opened = true
		}
		depth += o - c
		if depth <= 0 {
			return i
		}
	}
	return len(lines) - 1
}

func indentEnd(lines []string, start int) int {
	base := indent(lines[start])
	body := start
	for k := start; k < len(lines) && k < start+12; k++ {
		t := lines[k]
		if h := strings.Index(t, " #"); h >= 0 {
			t = t[:h]
		}
		if strings.HasSuffix(strings.TrimRight(t, " \t"), ":") {
			body = k
			break
		}
	}
	end := body
	for i := body + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if indent(lines[i]) <= base {
			break
		}
		end = i
	}
	return end
}

func endEnd(lines []string, start int) int {
	t0 := strings.TrimSpace(lines[start])
	if strings.HasSuffix(t0, " end") || strings.Contains(t0, "; end") || strings.Contains(t0, " do: ") || strings.Contains(t0, ", do:") {
		return start
	}
	base := indent(lines[start])
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		ind := indent(lines[i])
		if ind == base && (t == "end" || strings.HasPrefix(t, "end ") || strings.HasPrefix(t, "end)") || strings.HasPrefix(t, "end,") || strings.HasPrefix(t, "end.")) {
			return i
		}
		if ind < base {
			return i - 1
		}
	}
	return len(lines) - 1
}
