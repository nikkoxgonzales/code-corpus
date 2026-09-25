package app

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func errLine(b *strings.Builder, name string, e interface {
	Error() string
}, hint string) {
	fmt.Fprintf(b, "error %s: %s", name, e.Error())
	if hint != "" {
		fmt.Fprintf(b, " (fix: %s)", hint)
	}
	b.WriteByte('\n')
}

func (r *AddResult) Text() string {
	var b strings.Builder
	for _, it := range r.Items {
		switch it.Status {
		case "added":
			fmt.Fprintf(&b, "added %s  %s@%s  %d files, %d chunks  %.1fs\n", it.Name, it.Ref, short(it.SHA), it.Files, it.Chunks, it.Seconds)
		case "exists":
			fmt.Fprintf(&b, "exists %s  %s@%s  (already in corpus; corpus update %s to refresh)\n", it.Name, it.Ref, short(it.SHA), it.Name)
		default:
			errLine(&b, it.Source, it.Error, it.Error.Hint)
		}
	}
	if r.Next != "" {
		b.WriteString("# next: " + r.Next + "\n")
	}
	return b.String()
}

func (r *UpdateResult) Text() string {
	var b strings.Builder
	for _, it := range r.Items {
		switch it.Status {
		case "error":
			errLine(&b, it.Name, it.Error, it.Error.Hint)
		case "current", "pinned":
			fmt.Fprintf(&b, "%s %s  %s\n", it.Status, it.Name, short(it.NewSHA))
		default:
			fmt.Fprintf(&b, "%s %s  %s -> %s  files +%d ~%d -%d  %.1fs\n", it.Status, it.Name, short(it.OldSHA), short(it.NewSHA), it.Added, it.Changed, it.Removed, it.Seconds)
		}
	}
	return b.String()
}

func (r *RemoveResult) Text() string {
	return "removed " + strings.Join(r.Removed, ", ") + "\n"
}

func (r *ListResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d repos in %s\n", len(r.Repos), r.Root)
	if len(r.Repos) == 0 {
		b.WriteString("# next: corpus add <owner/repo>\n")
		return b.String()
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, it := range r.Repos {
		ref := it.Ref
		if it.Pinned {
			ref += "(pinned)"
		}
		fmt.Fprintf(tw, "%s\t%s@%s\t%s\t%d files\t%d chunks\t%s\t%s\n", it.Name, ref, short(it.SHA),
			it.UpdatedAt.Format("2006-01-02"), it.Files, it.Chunks, strings.Join(it.Langs, ","), it.URL)
	}
	tw.Flush()
	return b.String()
}

func (r *StatusResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# root %s  repos=%d  git=%s  ripgrep=%s\n# reranker: %s\n", r.Root, r.Repos, r.Git, r.Ripgrep, r.Reranker)
	if len(r.Issues) == 0 {
		b.WriteString("ok: no issues\n")
	}
	for _, i := range r.Issues {
		if i.Repo != "" {
			fmt.Fprintf(&b, "issue %s: %s (fix: %s)\n", i.Repo, i.Problem, i.Fix)
		} else {
			fmt.Fprintf(&b, "issue: %s (fix: %s)\n", i.Problem, i.Fix)
		}
	}
	return b.String()
}
