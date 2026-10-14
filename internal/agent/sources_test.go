package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/store"
)

func TestSources(t *testing.T) {
	demo := &repo{label: "urtuba/demo", resourceID: 1, commit: commitA}
	dev := &repo{label: "urtuba/demo@dev", resourceID: 2, commit: commitB}
	tb := &toolbox{repos: []*repo{demo, dev}, read: map[fileKey]int{
		{1, "app/refund.go"}: 40,
		{1, "README.md"}:     10,
		{2, "app/refund.go"}: 50,
	}}
	src := func(r *repo, p string, from, to int) store.NewSource {
		return store.NewSource{ResourceID: r.resourceID, Path: p, LineFrom: from, LineTo: to, CommitID: r.commit}
	}
	tests := []struct {
		name   string
		answer string
		want   []store.NewSource
	}{
		{"a range", "See urtuba/demo/app/refund.go:3-12.", []store.NewSource{src(demo, "app/refund.go", 3, 12)}},
		{"one line", "See urtuba/demo/README.md:4", []store.NewSource{src(demo, "README.md", 4, 4)}},
		{"in code, brackets and parentheses",
			"`urtuba/demo/app/refund.go:1-2`, [urtuba/demo/README.md:1] (urtuba/demo/app/refund.go:5 - 6)",
			[]store.NewSource{src(demo, "app/refund.go", 1, 2), src(demo, "README.md", 1, 1), src(demo, "app/refund.go", 5, 6)}},
		{"a long dash and other case", "URTUBA/Demo/app/refund.go:7\u201310",
			[]store.NewSource{src(demo, "app/refund.go", 7, 10)}},
		{"in the order they appear, once each",
			"urtuba/demo@dev/app/refund.go:1-2 then urtuba/demo/README.md:2 and urtuba/demo@dev/app/refund.go:1-2",
			[]store.NewSource{src(dev, "app/refund.go", 1, 2), src(demo, "README.md", 2, 2)}},
		{"a clean path", "urtuba/demo/app/./refund.go:3", []store.NewSource{src(demo, "app/refund.go", 3, 3)}},
		{"a file not read", "urtuba/demo/app/cancel.go:3", nil},
		{"past the end of the file", "urtuba/demo/README.md:9-11", nil},
		{"a range backwards", "urtuba/demo/app/refund.go:9-3", nil},
		{"line 0", "urtuba/demo/app/refund.go:0-3", nil},
		{"no lines", "urtuba/demo/app/refund.go", nil},
		{"a longer name", "xurtuba/demo/app/refund.go:3 and other/urtuba/demo/app/refund.go:3", nil},
		{"a path that leaves the clone", "urtuba/demo/../demo/app/refund.go:3", nil},
		{"a huge number", "urtuba/demo/app/refund.go:99999999999999999999", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tb.sources(tt.answer)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("sources(%q) =\n%+v\nwant\n%+v", tt.answer, got, tt.want)
			}
		})
	}

	// At most maxSources.
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "urtuba/demo/app/refund.go:%d ", i+1)
	}
	if got := tb.sources(b.String()); len(got) != maxSources {
		t.Errorf("got %d sources, want %d", len(got), maxSources)
	}
}

func TestSourceURL(t *testing.T) {
	src := store.Source{Owner: "urtuba", Repo: "demo", Path: "app/refund.go", LineFrom: 3, LineTo: 12, CommitID: commitA}
	tests := []struct {
		name   string
		change func(*store.Source)
		want   string
	}{
		{"a range", func(*store.Source) {},
			"https://github.com/urtuba/demo/blob/" + commitA + "/app/refund.go#L3-L12"},
		{"one line", func(s *store.Source) { s.LineTo = 3 },
			"https://github.com/urtuba/demo/blob/" + commitA + "/app/refund.go#L3"},
		{"signs in the path are escaped", func(s *store.Source) { s.Path = "docs/a b#c?d.md" },
			"https://github.com/urtuba/demo/blob/" + commitA + "/docs/a%20b%23c%3Fd.md#L3-L12"},
		{"a short commit", func(s *store.Source) { s.CommitID = "7fd1a60" }, ""},
		{"no commit", func(s *store.Source) { s.CommitID = "" }, ""},
		{"a branch name as commit", func(s *store.Source) { s.CommitID = "main/../../evil" }, ""},
	}
	for _, tt := range tests {
		s := src
		tt.change(&s)
		if got := SourceURL(s); got != tt.want {
			t.Errorf("%s: SourceURL() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestLimiterGiveBack(t *testing.T) {
	l := newLimiter()
	k := limitKey{1, 2}
	c := &clock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	for range 3 {
		if _, ok := l.take(k, 3, c.now()); !ok {
			t.Fatal("refused under the limit")
		}
		c.advance(time.Second)
	}
	if _, ok := l.take(k, 3, c.now()); ok {
		t.Fatal("passed over the limit")
	}
	l.giveBack(k, c.now().Add(-time.Second))
	if _, ok := l.take(k, 3, c.now()); !ok {
		t.Error("refused after a question was given back")
	}
}
