// Package index keeps the SQLite index: files, definition-level chunks, symbols
// and an FTS5 (BM25) table over identifier-expanded chunk text. Chunk text itself
// is not stored; excerpts are read from the checkout at query time.
package index

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/nikkoxgonzales/code-corpus/internal/chunk"
	"github.com/nikkoxgonzales/code-corpus/internal/gitops"
)

// File flags.
const (
	FlagTest      = 1
	FlagVendor    = 2
	FlagGenerated = 4
	FlagDoc       = 8
	FlagSkipped   = 16 // tracked but not indexed (binary, too large, minified)
)

const schema = `
CREATE TABLE IF NOT EXISTS files(
  id INTEGER PRIMARY KEY, repo TEXT NOT NULL, path TEXT NOT NULL, lang TEXT,
  blob TEXT, size INTEGER, flags INTEGER NOT NULL DEFAULT 0, UNIQUE(repo, path));
CREATE TABLE IF NOT EXISTS chunks(
  id INTEGER PRIMARY KEY, file_id INTEGER NOT NULL, start_line INTEGER, end_line INTEGER, symbol TEXT, kind TEXT);
CREATE INDEX IF NOT EXISTS chunks_file ON chunks(file_id, start_line);
CREATE TABLE IF NOT EXISTS symbols(
  id INTEGER PRIMARY KEY, file_id INTEGER NOT NULL, name TEXT, name_lower TEXT, kind TEXT, line INTEGER);
CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name_lower);
CREATE INDEX IF NOT EXISTS symbols_file ON symbols(file_id);
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
  symbol, path, body, content='', contentless_delete=1, tokenize='porter unicode61');
`

type DB struct {
	sql *sql.DB
}

func Open(file string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.ToSlash(file)+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; parallel repo updates serialize here
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init index: %w", err)
	}
	return &DB{sql: db}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

type Stats struct {
	Files   int `json:"files"`
	Chunks  int `json:"chunks"`
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
}

type parsed struct {
	e      gitops.Entry
	lang   string
	flags  int
	size   int
	res    chunk.Result
	bodies []string
}

// IndexRepo brings the index for repo in line with the checkout in dir.
// Only files whose git blob changed are re-parsed.
func (d *DB) IndexRepo(repo, dir string, entries []gitops.Entry) (Stats, error) {
	var st Stats
	type old struct {
		id   int64
		blob string
	}
	existing := map[string]old{}
	rows, err := d.sql.Query(`SELECT id, path, blob FROM files WHERE repo = ?`, repo)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var o old
		var p string
		if err := rows.Scan(&o.id, &p, &o.blob); err != nil {
			rows.Close()
			return st, err
		}
		existing[p] = o
	}
	rows.Close()

	keep := map[string]bool{}
	var todo []gitops.Entry
	for _, e := range entries {
		if skipPath(e.Path) || chunk.Lang(e.Path) == "" {
			continue
		}
		keep[e.Path] = true
		o, ok := existing[e.Path]
		if ok && o.blob == e.Blob {
			continue
		}
		if ok {
			st.Changed++
		} else {
			st.Added++
		}
		todo = append(todo, e)
	}
	var del []int64
	for p, o := range existing {
		if !keep[p] {
			st.Removed++
			del = append(del, o.id)
		}
	}
	for _, e := range todo {
		if o, ok := existing[e.Path]; ok {
			del = append(del, o.id)
		}
	}

	// Parse in parallel, write from one goroutine.
	in := make(chan gitops.Entry)
	out := make(chan parsed, 64)
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range in {
				out <- parseFile(dir, e)
			}
		}()
	}
	go func() {
		for _, e := range todo {
			in <- e
		}
		close(in)
		wg.Wait()
		close(out)
	}()

	tx, err := d.sql.Begin()
	if err != nil {
		drain(out)
		return st, err
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	for _, id := range del {
		if err := deleteFile(tx, id); err != nil {
			drain(out)
			return st, err
		}
	}
	n := 0
	for p := range out {
		if err := insertFile(tx, repo, p); err != nil {
			drain(out)
			return st, fmt.Errorf("index %s: %w", p.e.Path, err)
		}
		if n++; n%400 == 0 {
			if err := tx.Commit(); err != nil {
				drain(out)
				return st, err
			}
			if tx, err = d.sql.Begin(); err != nil {
				drain(out)
				return st, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	tx = nil
	st.Files, st.Chunks = d.counts(repo)
	return st, nil
}

func drain(ch chan parsed) {
	for range ch {
	}
}

func (d *DB) counts(repo string) (files, chunks int) {
	d.sql.QueryRow(`SELECT COUNT(*) FROM files WHERE repo = ? AND flags & 16 = 0`, repo).Scan(&files)
	d.sql.QueryRow(`SELECT COUNT(*) FROM chunks c JOIN files f ON f.id = c.file_id WHERE f.repo = ?`, repo).Scan(&chunks)
	return
}

func deleteFile(tx *sql.Tx, id int64) error {
	for _, q := range []string{
		`DELETE FROM chunks_fts WHERE rowid IN (SELECT id FROM chunks WHERE file_id = ?)`,
		`DELETE FROM chunks WHERE file_id = ?`,
		`DELETE FROM symbols WHERE file_id = ?`,
		`DELETE FROM files WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

func insertFile(tx *sql.Tx, repo string, p parsed) error {
	r, err := tx.Exec(`INSERT INTO files(repo, path, lang, blob, size, flags) VALUES(?,?,?,?,?,?)`,
		repo, p.e.Path, p.lang, p.e.Blob, p.size, p.flags)
	if err != nil {
		return err
	}
	fid, _ := r.LastInsertId()
	if p.flags&FlagSkipped != 0 {
		return nil
	}
	pathTerms := Expand(p.e.Path)
	for i, c := range p.res.Chunks {
		r, err := tx.Exec(`INSERT INTO chunks(file_id, start_line, end_line, symbol, kind) VALUES(?,?,?,?,?)`,
			fid, c.Start, c.End, c.Symbol, c.Kind)
		if err != nil {
			return err
		}
		cid, _ := r.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO chunks_fts(rowid, symbol, path, body) VALUES(?,?,?,?)`,
			cid, Expand(c.Symbol), pathTerms, p.bodies[i]); err != nil {
			return err
		}
	}
	for _, s := range p.res.Symbols {
		if _, err := tx.Exec(`INSERT INTO symbols(file_id, name, name_lower, kind, line) VALUES(?,?,?,?,?)`,
			fid, s.Name, strings.ToLower(s.Name), s.Kind, s.Line); err != nil {
			return err
		}
	}
	return nil
}

func parseFile(dir string, e gitops.Entry) parsed {
	p := parsed{e: e, lang: chunk.Lang(e.Path), flags: PathFlags(e.Path)}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
	p.size = len(b)
	if err != nil || skipContent(p.lang, b) {
		p.flags |= FlagSkipped
		return p
	}
	if generatedHeader(b) {
		p.flags |= FlagGenerated
	}
	src := string(b)
	p.res = chunk.Split(p.lang, src)
	lines := chunk.SplitLines(src)
	for _, c := range p.res.Chunks {
		p.bodies = append(p.bodies, Expand(strings.Join(lines[c.Start-1:c.End], "\n")))
	}
	return p
}

var skipDirs = map[string]bool{
	"node_modules": true, "bower_components": true, "__pycache__": true, ".venv": true, "venv": true,
	".tox": true, ".mypy_cache": true, ".next": true, ".nuxt": true, ".git": true, ".yarn": true,
}
var lockfiles = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "cargo.lock": true, "go.sum": true,
	"poetry.lock": true, "composer.lock": true, "gemfile.lock": true, "pipfile.lock": true, "bun.lockb": true, "uv.lock": true,
}

func skipPath(p string) bool {
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		if skipDirs[s] {
			return true
		}
	}
	base := strings.ToLower(segs[len(segs)-1])
	return lockfiles[base] || strings.Contains(base, ".min.") || strings.HasSuffix(base, ".map")
}

// PathFlags classifies a path as test, vendor, generated or docs.
func PathFlags(p string) int {
	f := 0
	lp := strings.ToLower(p)
	segs := strings.Split(lp, "/")
	base := segs[len(segs)-1]
	for _, s := range segs[:len(segs)-1] {
		switch s {
		case "vendor", "third_party", "third-party", "thirdparty", "external", "extern", "deps":
			f |= FlagVendor
		case "test", "tests", "__tests__", "testdata", "testing", "spec", "specs", "e2e", "fixtures", "__mocks__", "mocks", "benchmarks":
			f |= FlagTest
		case "docs", "doc", "documentation", "examples", "example":
			f |= FlagDoc
		case "generated", "gen", "__generated__":
			f |= FlagGenerated
		}
	}
	orig := path.Base(p)
	if strings.Contains(base, "_test.") || strings.HasPrefix(base, "test_") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.Contains(base, "_spec.") ||
		strings.HasSuffix(strings.TrimSuffix(orig, path.Ext(orig)), "Test") || strings.HasSuffix(strings.TrimSuffix(orig, path.Ext(orig)), "Tests") {
		f |= FlagTest
	}
	if strings.Contains(base, ".pb.") || strings.Contains(base, "_pb2") || strings.Contains(base, ".generated.") ||
		strings.Contains(base, ".gen.") || strings.Contains(base, "_generated.") || strings.HasSuffix(base, ".g.dart") {
		f |= FlagGenerated
	}
	switch chunk.Lang(p) {
	case "markdown", "text":
		f |= FlagDoc
	}
	return f
}

func skipContent(lang string, b []byte) bool {
	if len(b) > 1<<20 || len(b) == 0 {
		return true
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return true
	}
	if lang == "config" && len(b) > 256<<10 {
		return true
	}
	nl := bytes.Count(b, []byte{'\n'}) + 1
	return len(b) > 2000 && len(b)/nl > 250 // minified or data blob
}

func generatedHeader(b []byte) bool {
	head := b[:min(len(b), 600)]
	for _, m := range []string{"Code generated", "DO NOT EDIT", "@generated", "auto-generated", "autogenerated", "Autogenerated"} {
		if bytes.Contains(head, []byte(m)) {
			return true
		}
	}
	return false
}

func (d *DB) DeleteRepo(repo string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM chunks_fts WHERE rowid IN (SELECT c.id FROM chunks c JOIN files f ON f.id = c.file_id WHERE f.repo = ?)`,
		`DELETE FROM chunks WHERE file_id IN (SELECT id FROM files WHERE repo = ?)`,
		`DELETE FROM symbols WHERE file_id IN (SELECT id FROM files WHERE repo = ?)`,
		`DELETE FROM files WHERE repo = ?`,
	} {
		if _, err := tx.Exec(q, repo); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- queries ----

type Filter struct {
	Repos []string
	Langs []string
	Path  string // glob, see GlobPattern
}

// GlobPattern converts a user path filter to an SQLite GLOB pattern.
// No wildcard means "path contains"; ** and * both match across directories.
func GlobPattern(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if !strings.ContainsAny(p, "*?[") {
		return "*" + p + "*"
	}
	for strings.Contains(p, "**") {
		p = strings.ReplaceAll(p, "**", "*")
	}
	return p
}

func (f Filter) where() (string, []any) {
	var cl []string
	var args []any
	if len(f.Repos) > 0 {
		cl = append(cl, "f.repo IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Repos)), ",")+")")
		for _, r := range f.Repos {
			args = append(args, r)
		}
	}
	if len(f.Langs) > 0 {
		cl = append(cl, "f.lang IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Langs)), ",")+")")
		for _, l := range f.Langs {
			args = append(args, strings.ToLower(l))
		}
	}
	if f.Path != "" {
		cl = append(cl, "f.path GLOB ?")
		args = append(args, GlobPattern(f.Path))
	}
	if len(cl) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(cl, " AND "), args
}

type Cand struct {
	ChunkID    int64
	Repo, Path string
	Lang       string
	Flags      int
	Start, End int
	Symbol     string
	Kind       string
	BM25       float64 // higher is better
}

// BM25 returns the best chunks for an FTS5 match expression.
func (d *DB) BM25(match string, f Filter, limit int) ([]Cand, error) {
	w, args := f.where()
	q := `SELECT c.id, f.repo, f.path, f.lang, f.flags, c.start_line, c.end_line, c.symbol, c.kind,
	        bm25(chunks_fts, 4.0, 2.0, 1.0) AS r
	      FROM chunks_fts JOIN chunks c ON c.id = chunks_fts.rowid JOIN files f ON f.id = c.file_id
	      WHERE chunks_fts MATCH ?` + w + ` ORDER BY r LIMIT ?`
	rows, err := d.sql.Query(q, append(append([]any{match}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cand
	for rows.Next() {
		var c Cand
		if err := rows.Scan(&c.ChunkID, &c.Repo, &c.Path, &c.Lang, &c.Flags, &c.Start, &c.End, &c.Symbol, &c.Kind, &c.BM25); err != nil {
			return nil, err
		}
		c.BM25 = -c.BM25
		out = append(out, c)
	}
	return out, rows.Err()
}

type SymHit struct {
	Repo, Path, Lang string
	Flags            int
	Name, Kind       string
	Line             int
	Start, End       int // enclosing chunk
	Match            int // 0 exact, 1 case-insensitive, 2 prefix, 3 contains
}

// FindSymbols looks up definitions by name: exact, then prefix, then substring.
func (d *DB) FindSymbols(name string, f Filter, limit int) ([]SymHit, error) {
	low := strings.ToLower(name)
	if i := strings.LastIndexAny(low, ".:"); i >= 0 && i < len(low)-1 {
		low = low[i+1:]
	}
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(low)
	w, args := f.where()
	q := `SELECT f.repo, f.path, f.lang, f.flags, s.name, s.kind, s.line,
	        COALESCE((SELECT c.start_line FROM chunks c WHERE c.file_id = s.file_id AND c.start_line <= s.line AND c.end_line >= s.line ORDER BY c.start_line DESC LIMIT 1), s.line),
	        COALESCE((SELECT c.end_line FROM chunks c WHERE c.file_id = s.file_id AND c.start_line <= s.line AND c.end_line >= s.line ORDER BY c.start_line DESC LIMIT 1), s.line),
	        CASE WHEN s.name = ? THEN 0 WHEN s.name_lower = ? THEN 1 WHEN s.name_lower LIKE ? ESCAPE '\' THEN 2 ELSE 3 END AS m
	      FROM symbols s JOIN files f ON f.id = s.file_id
	      WHERE s.name_lower LIKE ? ESCAPE '\'` + w + `
	      ORDER BY m, (f.flags & 7) != 0, length(s.name), f.repo, f.path, s.line LIMIT ?`
	all := []any{name, low, like + "%", "%" + like + "%"}
	rows, err := d.sql.Query(q, append(append(all, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SymHit
	for rows.Next() {
		var h SymHit
		if err := rows.Scan(&h.Repo, &h.Path, &h.Lang, &h.Flags, &h.Name, &h.Kind, &h.Line, &h.Start, &h.End, &h.Match); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// FileChunks returns the chunks of one file, ordered by start line.
func (d *DB) FileChunks(repo, p string) ([]Cand, error) {
	rows, err := d.sql.Query(`SELECT c.id, f.lang, f.flags, c.start_line, c.end_line, c.symbol, c.kind
	  FROM chunks c JOIN files f ON f.id = c.file_id WHERE f.repo = ? AND f.path = ? ORDER BY c.start_line`, repo, p)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cand
	for rows.Next() {
		c := Cand{Repo: repo, Path: p}
		if err := rows.Scan(&c.ChunkID, &c.Lang, &c.Flags, &c.Start, &c.End, &c.Symbol, &c.Kind); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type RepoStat struct {
	Files, Chunks int
	Langs         []string
}

func (d *DB) RepoStats() (map[string]*RepoStat, error) {
	out := map[string]*RepoStat{}
	get := func(r string) *RepoStat {
		if out[r] == nil {
			out[r] = &RepoStat{}
		}
		return out[r]
	}
	rows, err := d.sql.Query(`SELECT repo, COUNT(*) FROM files WHERE flags & 16 = 0 GROUP BY repo`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r string
		var n int
		rows.Scan(&r, &n)
		get(r).Files = n
	}
	rows.Close()
	rows, err = d.sql.Query(`SELECT f.repo, COUNT(*) FROM chunks c JOIN files f ON f.id = c.file_id GROUP BY f.repo`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r string
		var n int
		rows.Scan(&r, &n)
		get(r).Chunks = n
	}
	rows.Close()
	rows, err = d.sql.Query(`SELECT repo, lang, COUNT(*) AS n FROM files
	  WHERE flags & 16 = 0 AND lang NOT IN ('config','text','markdown','html','css')
	  GROUP BY repo, lang ORDER BY repo, n DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r, l string
		var n int
		rows.Scan(&r, &l, &n)
		if s := get(r); len(s.Langs) < 3 {
			s.Langs = append(s.Langs, l)
		}
	}
	return out, rows.Err()
}
