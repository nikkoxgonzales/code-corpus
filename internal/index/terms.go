package index

import (
	"strings"
	"unicode"
)

var stopwords = map[string]bool{}

var abbrev = map[string][]string{}

func init() {
	pairs := `receive:recv message:msg maximum:max minimum:min configuration:config,cfg config:cfg request:req
	response:resp,res error:err context:ctx initialize:init initialization:init parameter:param,params argument:arg,args
	directory:dir reference:ref temporary:tmp,temp number:num length:len string:str buffer:buf
	authentication:auth authorization:auth authenticate:auth connection:conn address:addr source:src
	destination:dst,dest database:db allocate:alloc previous:prev current:cur,curr index:idx count:cnt
	value:val variable:var library:lib package:pkg environment:env specification:spec command:cmd
	sequence:seq execute:exec synchronize:sync synchronous:sync asynchronous:async information:info
	iterator:iter iterate:iter document:doc object:obj attribute:attr expression:expr position:pos
	character:char integer:int boolean:bool function:fn,func callback:cb certificate:cert
	credential:cred,creds password:pwd,passwd transaction:tx,txn manager:mgr millisecond:ms
	calculate:calc compare:cmp utility:util,utils statistic:stats administrator:admin`
	for _, p := range strings.Fields(pairs) {
		long, shorts, _ := strings.Cut(p, ":")
		for _, s := range strings.Split(shorts, ",") {
			abbrev[long] = append(abbrev[long], s)
			abbrev[s] = append(abbrev[s], long)
		}
	}
	// Stemmed keys so "messages"/"receiving" hit too.
	for k, v := range abbrev {
		if s := stem(k); s != k {
			abbrev[s] = append(abbrev[s], v...)
		}
	}
}

var particles = map[string]bool{"in": true, "on": true, "out": true, "up": true, "off": true, "down": true, "back": true}

func init() {
	for _, w := range strings.Fields(`a an the of to in on at for and or but not no is are was were be been being it its
		this that these those there here how what where when which who whom why does do did done can could should would
		will shall may might must i me my we our you your they them their he she his her us
		with by from into onto about as than then so such via using use used uses
		code file files function functions method methods logic implementation implement implemented
		find show get gets where's whats thing things some any all way ways work works`) {
		stopwords[w] = true
	}
}

// identifiers splits text into identifier-like tokens (letters, digits, _ and $).
func identifiers(s string, fn func(tok string)) {
	start := -1
	for i, r := range s {
		ok := r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
		if ok && start < 0 {
			start = i
		} else if !ok && start >= 0 {
			fn(s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		fn(s[start:])
	}
}

// parts splits an identifier on _, $, and camelCase boundaries:
// "parseHTTPConfig" -> parse, http, config.
func parts(tok string) []string {
	var out []string
	rs := []rune(tok)
	cur := []rune{}
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range rs {
		if r == '_' || r == '$' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// Expand turns code into FTS text: every identifier is emitted lowercased,
// followed by its camel/snake parts, so "parseConfig" matches "parse config".
func Expand(s string) string {
	var b strings.Builder
	identifiers(s, func(tok string) {
		low := strings.ToLower(tok)
		b.WriteString(low)
		b.WriteByte(' ')
		if ps := parts(tok); len(ps) > 1 {
			for _, p := range ps {
				if len(p) > 1 {
					b.WriteString(p)
					b.WriteByte(' ')
				}
			}
		}
	})
	return b.String()
}

// QueryTerms extracts search terms from a natural-language or identifier query.
func QueryTerms(q string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if len(t) < 2 || stopwords[t] || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	var words []string
	identifiers(q, func(tok string) {
		words = append(words, strings.ToLower(tok))
		add(strings.ToLower(tok))
		if ps := parts(tok); len(ps) > 1 {
			for _, p := range ps {
				add(p)
			}
		}
	})
	// Code abbreviations in both directions: "receive message" also finds recvMsg.
	for _, w := range words {
		if stopwords[w] {
			continue
		}
		for _, a := range abbrev[stem(w)] {
			add(a)
		}
		for _, a := range abbrev[w] {
			add(a)
		}
	}
	// Compounds: "backed off" -> backoff, "time out" -> timeout, "rate limit" -> ratelimit.
	for i := 0; i+1 < len(words); i++ {
		a, b := stem(words[i]), stem(words[i+1])
		second := !stopwords[words[i+1]] || particles[words[i+1]] // "log in" -> login
		if len(a) >= 2 && len(b) >= 2 && !stopwords[words[i]] && second {
			add(a + b)
		}
	}
	return out
}

// stem strips common English inflections, enough to join compounds.
func stem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if len(w) > len(suf)+2 && strings.HasSuffix(w, suf) {
			w = strings.TrimSuffix(w, suf)
			if suf == "ed" && len(w) > 2 && w[len(w)-1] == w[len(w)-2] { // "stopped" -> "stop"
				w = w[:len(w)-1]
			}
			return w
		}
	}
	return w
}

// MatchExpr builds an FTS5 OR-query from terms.
func MatchExpr(terms []string) string {
	q := make([]string, len(terms))
	for i, t := range terms {
		q[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}
	return strings.Join(q, " OR ")
}
