package chunk

import "testing"

func find(r Result, sym string) *Chunk {
	for i := range r.Chunks {
		if r.Chunks[i].Symbol == sym && r.Chunks[i].Kind != "block" {
			return &r.Chunks[i]
		}
	}
	return nil
}

func TestGoFunctionsAndConstDoNotMerge(t *testing.T) {
	src := `package x

const name = "a"

// Foo does foo.
func Foo() {
	if true {
		println("}")
	}
}

func (s *S) Bar(x int) int {
	return x
}
`
	r := Split("go", src)
	foo := find(r, "Foo")
	if foo == nil || foo.Start != 5 || foo.End != 10 {
		t.Fatalf("Foo chunk = %+v, want lines 5-10 (with doc comment)", foo)
	}
	if bar := find(r, "Bar"); bar == nil || bar.Kind != "method" || bar.End != 14 {
		t.Fatalf("Bar chunk = %+v", bar)
	}
	if c := find(r, "name"); c != nil {
		t.Fatalf("one-line const became its own chunk: %+v", c)
	}
}

func TestPythonIndent(t *testing.T) {
	src := "class A:\n    def m(self):\n        return 1\n\n    def n(self):\n        pass\n\ndef top():\n    x = 1\n    return x\n"
	r := Split("python", src)
	if a := find(r, "A"); a == nil || a.End != 6 {
		t.Fatalf("class A = %+v", a)
	}
	if top := find(r, "top"); top == nil || top.Start != 8 || top.End != 10 {
		t.Fatalf("top = %+v", top)
	}
	var kinds = map[string]string{}
	for _, s := range r.Symbols {
		kinds[s.Name] = s.Kind
	}
	if kinds["m"] != "method" || kinds["top"] != "func" {
		t.Fatalf("symbol kinds = %v", kinds)
	}
}

func TestJSMemberAssignmentAndNoCallFalsePositive(t *testing.T) {
	src := `res.sendFile = function sendFile(path, cb) {
  var x = 1;
  router(req, res, function (err) {
    done();
  });
};
`
	r := Split("javascript", src)
	if c := find(r, "sendFile"); c == nil || c.End != 6 {
		t.Fatalf("sendFile = %+v", c)
	}
	for _, s := range r.Symbols {
		if s.Name == "router" {
			t.Fatal("call site router(...) recorded as a definition")
		}
	}
}

func TestRustLifetimesDoNotBreakBraces(t *testing.T) {
	src := "impl<'a> Foo<'a> {\n    fn get(&self) -> &'a str {\n        self.x\n    }\n}\n"
	r := Split("rust", src)
	if c := find(r, "Foo"); c == nil || c.End != 5 {
		t.Fatalf("impl Foo = %+v", c)
	}
}

func TestLargeDefinitionIsSplit(t *testing.T) {
	src := "func Big() {\n"
	for i := 0; i < 400; i++ {
		src += "\tx := 1\n"
	}
	src += "}\n"
	r := Split("go", src)
	if len(r.Chunks) < 3 {
		t.Fatalf("400-line function produced %d chunks", len(r.Chunks))
	}
	for _, c := range r.Chunks {
		if c.End-c.Start+1 > maxChunk {
			t.Fatalf("chunk too large: %+v", c)
		}
		if c.Symbol != "Big" {
			t.Fatalf("split chunk lost its symbol: %+v", c)
		}
	}
}

func TestMarkdownSections(t *testing.T) {
	r := Split("markdown", "# Title\nintro\nmore\n\n## Install\nrun it\nthen this\n```\n# not a heading\n```\n")
	if c := find(r, "Install"); c == nil || c.Start != 5 || c.End != 10 {
		t.Fatalf("Install section = %+v", c)
	}
}

func TestLang(t *testing.T) {
	for p, want := range map[string]string{"a/b.go": "go", "x.TSX": "typescript", "Makefile": "config", "img.png": ""} {
		if got := Lang(p); got != want {
			t.Errorf("Lang(%q) = %q, want %q", p, got, want)
		}
	}
}
