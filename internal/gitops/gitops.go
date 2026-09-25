// Package gitops wraps the git CLI: shallow clones, updates and file listings.
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/nikkoxgonzales/code-corpus/internal/xerr"
)

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// Common -c flags: long Windows paths, no line-ending rewriting.
var baseArgs = []string{"-c", "core.longpaths=true", "-c", "core.autocrlf=false", "-c", "advice.detachedHead=false"}

func run(dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, baseArgs...), args...)...)
	cmd.Dir = dir
	// Never block on credential prompts: agents cannot answer them.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS=")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			msg = "timed out after " + timeout.String()
		}
		if i := strings.LastIndex(msg, "\n"); i >= 0 && strings.Contains(msg[i:], "fatal") {
			msg = msg[i+1:]
		}
		return "", xerr.New(xerr.Network, gitHint(msg), "git %s: %s", args[0], firstLine(msg))
	}
	return out.String(), nil
}

func gitHint(msg string) string {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "not found") || strings.Contains(l, "could not read username") || strings.Contains(l, "authentication"):
		return "check the URL; private repos need git credentials configured outside corpus"
	case strings.Contains(l, "remote branch") || strings.Contains(l, "couldn't find remote ref"):
		return "ref does not exist; omit @ref to track the default branch"
	case strings.Contains(l, "timed out") || strings.Contains(l, "could not resolve host"):
		return "network problem; retry later"
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Clone makes a shallow clone of url at ref (branch, tag or full commit SHA; "" = default branch).
// It returns the ref actually tracked and whether it is pinned (tag or commit).
func Clone(url, ref, dir string) (tracked string, pinned bool, err error) {
	if shaRe.MatchString(ref) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false, err
		}
		for _, a := range [][]string{{"init", "-q"}, {"remote", "add", "origin", url}, {"fetch", "-q", "--depth", "1", "origin", ref}, {"checkout", "-q", "FETCH_HEAD"}} {
			if _, err := run(dir, 10*time.Minute, a...); err != nil {
				return "", false, err
			}
		}
		return ref, true, nil
	}
	args := []string{"clone", "-q", "--depth", "1", "--single-branch", "--no-tags"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, dir)
	if _, err := run("", 15*time.Minute, args...); err != nil {
		os.RemoveAll(dir)
		return "", false, err
	}
	if ref == "" {
		b, err := run(dir, time.Minute, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", false, err
		}
		return strings.TrimSpace(b), false, nil
	}
	// A tag checks out as detached HEAD; a branch does not.
	_, err = run(dir, time.Minute, "symbolic-ref", "-q", "HEAD")
	return ref, err != nil, nil
}

// Update fetches the latest commit of ref and hard-resets to it.
func Update(dir, ref string) error {
	if _, err := run(dir, 15*time.Minute, "fetch", "-q", "--depth", "1", "--no-tags", "origin", ref); err != nil {
		return err
	}
	_, err := run(dir, 5*time.Minute, "reset", "-q", "--hard", "FETCH_HEAD")
	return err
}

func HeadSHA(dir string) (string, error) {
	b, err := run(dir, time.Minute, "rev-parse", "HEAD")
	return strings.TrimSpace(b), err
}

type Entry struct {
	Path string // slash-separated, relative to repo root
	Blob string // git blob sha: changes iff content changes
}

// LsFiles lists tracked files with their blob hashes.
func LsFiles(dir string) ([]Entry, error) {
	out, err := run(dir, 5*time.Minute, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var es []Entry
	for _, rec := range strings.Split(out, "\x00") {
		// "<mode> <blob> <stage>\t<path>"
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(rec[:tab])
		if len(f) < 3 || f[0] == "160000" { // skip submodules
			continue
		}
		es = append(es, Entry{Path: rec[tab+1:], Blob: f[1]})
	}
	return es, nil
}

func Available() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found on PATH")
	}
	return nil
}
