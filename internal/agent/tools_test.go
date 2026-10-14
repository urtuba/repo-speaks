package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
)

// toolbox opens the agent's repositories, as an answer does.
func (f *fixture) toolbox(a store.Agent) *toolbox {
	f.t.Helper()
	agent := must(f.st.Agent(f.ctx, f.project.ID, a.ID))
	t, skipped, err := f.svc.openRepos(f.ctx, f.project.ID, agent)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(skipped) > 0 {
		f.t.Fatalf("skipped %v", skipped)
	}
	f.t.Cleanup(t.close)
	return t
}

// call runs one tool call and returns the step and the result.
func call(t *testing.T, tb *toolbox, name, args string) (string, string) {
	t.Helper()
	step, result, err := tb.run(context.Background(), llm.ToolCall{ID: "1", Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return step, result
}

func TestListFiles(t *testing.T) {
	f := newFixture(t)
	tb := f.toolbox(f.chat)
	tests := []struct {
		args, step, want string
	}{
		{`{"path": "urtuba/demo"}`, "Listing urtuba/demo",
			"Contents of urtuba/demo/:\nREADME.md\napp/\nassets/\ndata/\ndocs/\nvendor/\n"},
		{`{"path": "/urtuba/demo/app/"}`, "Listing /urtuba/demo/app/",
			"Contents of urtuba/demo/app/:\ncancel.go\ndeep/\nrefund.go\n"},
		{`{"path": "URTUBA/Docs"}`, "Listing URTUBA/Docs", "Contents of urtuba/docs/:\nREADME.md\nhow-to.md\nsecret.txt\n"},
		{`{"path": "urtuba/demo/app/refund.go"}`, "Listing urtuba/demo/app/refund.go",
			"Error: urtuba/demo/app/refund.go is a file. Use read_file to read it."},
		{`{"path": "urtuba/demo/nope"}`, "Listing urtuba/demo/nope", "Error: urtuba/demo/nope does not exist."},
		{`{"path": "urtuba/demox"}`, "Listing urtuba/demox",
			`Error: "urtuba/demox" is in none of your repositories. Paths start with one of: urtuba/demo, urtuba/docs.`},
		{`{"path": `, "Listing", "Error: the arguments are not valid JSON."},
	}
	for _, tt := range tests {
		step, got := call(t, tb, "list_files", tt.args)
		if step != tt.step || got != tt.want {
			t.Errorf("list_files %s = %q, %q\nwant %q, %q", tt.args, step, got, tt.step, tt.want)
		}
	}
	if _, got := call(t, tb, "delete_files", `{}`); got != `Error: there is no tool named "delete_files".` {
		t.Errorf("unknown tool: %q", got)
	}

	// A long folder is capped.
	dir := filepath.Join(f.cloneDir(f.demo), "many")
	os.Mkdir(dir, 0o750)
	for i := range maxListEntries + 5 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), nil, 0o600)
	}
	_, got := call(t, tb, "list_files", `{"path": "urtuba/demo/many"}`)
	if n := strings.Count(got, ".txt\n"); n != maxListEntries || !strings.HasSuffix(got, "[5 more not shown]\n") {
		t.Errorf("long folder: %d entries, ends %q", n, got[len(got)-30:])
	}
}

func TestSearchText(t *testing.T) {
	f := newFixture(t)
	tb := f.toolbox(f.chat)
	tests := []struct {
		args, step, want string
	}{
		// No case, both repositories, binary and non-UTF-8 files skipped.
		{`{"query": "Cancel", "path": "urtuba/demo/app"}`, "Searching for Cancel in urtuba/demo/app",
			"urtuba/demo/app/cancel.go:3: // Clients cannot CANCEL after 24 hours.\nurtuba/demo/app/cancel.go:4: const cancelHours = 24\n"},
		{`{"query": "to cancel"}`, "Searching for to cancel",
			"urtuba/demo/docs/guide.md:1: Ask support to cancel an order.\nurtuba/docs/how-to.md:1: To cancel, write to support.\n"},
		// A * matches any text inside one line.
		{`{"query": "clients*24"}`, "Searching for clients*24",
			"urtuba/demo/app/cancel.go:3: // Clients cannot CANCEL after 24 hours.\n"},
		{`{"query": "nowhere to be found"}`, "Searching for nowhere to be found", `No lines contain "nowhere to be found".`},
		{`{"query": " * "}`, "Searching for *", "Error: the query is empty."},
		{`{"query": "x", "path": "urtuba/demo/../../.."}`, "Searching for x in urtuba/demo/../../..",
			"Error: urtuba/demo/../../.. leads outside the repository."},
		{`{"query": "x", "path": "urtuba/billing"}`, "Searching for x in urtuba/billing",
			`Error: "urtuba/billing" is in none of your repositories. Paths start with one of: urtuba/demo, urtuba/docs.`},
	}
	for _, tt := range tests {
		step, got := call(t, tb, "search_text", tt.args)
		if step != tt.step || got != tt.want {
			t.Errorf("search_text %s = %q, %q\nwant %q, %q", tt.args, step, got, tt.step, tt.want)
		}
	}

	// Hits are capped.
	_, got := call(t, tb, "search_text", `{"query": "cancel line"}`)
	if n := strings.Count(got, "\n"); n != maxHits+1 || !strings.Contains(got, "[The search stopped at its limit.") {
		t.Errorf("many hits: %d lines\n%s", n, got)
	}
	// A long line is cut.
	os.WriteFile(filepath.Join(f.cloneDir(f.demo), "long.txt"), []byte("needle "+strings.Repeat("x", 1000)), 0o600)
	_, got = call(t, tb, "search_text", `{"query": "needle"}`)
	if !strings.HasSuffix(got, " [line cut]\n") || len(got) > maxHitLen+100 {
		t.Errorf("long line: %q", got)
	}
}

func TestReadFile(t *testing.T) {
	f := newFixture(t)
	tb := f.toolbox(f.chat)
	lines := make([]string, 950)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	os.WriteFile(filepath.Join(f.cloneDir(f.demo), "long.txt"), []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o600)
	os.WriteFile(filepath.Join(f.cloneDir(f.demo), "empty.txt"), nil, 0o600)

	tests := []struct {
		args, want string
	}{
		{`{"path": "urtuba/demo/app/refund.go"}`,
			"urtuba/demo/app/refund.go, lines 1 to 4 of 4:\n1: package app\n2: \n3: // Refund pays a client back within 5 days.\n4: func Refund() {}\n"},
		{`{"path": "urtuba/demo/app/./refund.go", "from_line": 3, "to_line": 3}`,
			"urtuba/demo/app/refund.go, lines 3 to 3 of 4:\n3: // Refund pays a client back within 5 days.\n"},
		{`{"path": "urtuba/demo/app/refund.go", "from_line": 4, "to_line": 99}`,
			"urtuba/demo/app/refund.go, lines 4 to 4 of 4:\n4: func Refund() {}\n"},
		{`{"path": "urtuba/demo/app/refund.go", "from_line": 9}`, "Error: urtuba/demo/app/refund.go has only 4 lines."},
		{`{"path": "urtuba/demo/app/refund.go", "from_line": 3, "to_line": 2}`, "Error: to_line 2 is before from_line 3."},
		{`{"path": "urtuba/demo/empty.txt"}`, "urtuba/demo/empty.txt is empty.\n"},
		{`{"path": "urtuba/demo/app"}`, "Error: urtuba/demo/app is a folder. Use list_files to see what is in it."},
		{`{"path": "urtuba/demo/assets/logo.png"}`, "Error: urtuba/demo/assets/logo.png is a binary file, not text, so it cannot be read."},
		{`{"path": "urtuba/demo/data/latin1.txt"}`, "Error: urtuba/demo/data/latin1.txt is not UTF-8 text, so it cannot be read."},
		{`{"path": "urtuba/demo/missing.go"}`, "Error: urtuba/demo/missing.go does not exist."},
	}
	for _, tt := range tests {
		if _, got := call(t, tb, "read_file", tt.args); got != tt.want {
			t.Errorf("read_file %s =\n%q\nwant\n%q", tt.args, got, tt.want)
		}
	}

	// At most 400 lines per call, with a note on where to go on.
	_, got := call(t, tb, "read_file", `{"path": "urtuba/demo/long.txt", "from_line": 101, "to_line": 900}`)
	if !strings.HasPrefix(got, "urtuba/demo/long.txt, lines 101 to 500 of 950:\n101: line 101\n") ||
		!strings.HasSuffix(got, "500: line 500\n[At most 400 lines per call. Read on from line 501.]\n") {
		t.Errorf("long read: starts %q, ends %q", got[:60], got[len(got)-80:])
	}
	// Every file read counts, under its clean path. Failed reads do not.
	want := map[fileKey]int{
		{f.demo.ID, "app/refund.go"}: 4,
		{f.demo.ID, "empty.txt"}:     0,
		{f.demo.ID, "long.txt"}:      950,
	}
	if fmt.Sprint(tb.read) != fmt.Sprint(want) {
		t.Errorf("read = %v, want %v", tb.read, want)
	}

	// The output of one call is capped.
	os.WriteFile(filepath.Join(f.cloneDir(f.demo), "wide.txt"), []byte(strings.Repeat(strings.Repeat("y", 400)+"\n", 400)), 0o600)
	_, got = call(t, tb, "read_file", `{"path": "urtuba/demo/wide.txt"}`)
	if len(got) > maxOutputBytes+100 || !strings.HasSuffix(got, "[Output cut. Ask for less, such as fewer lines.]") {
		t.Errorf("wide read: %d bytes, ends %q", len(got), got[len(got)-60:])
	}
}

// TestToolsStayInsideTheClone plants links in a clone that point at a
// fake database file next to the clones, and paths with "..". No tool
// may read the file.
func TestToolsStayInsideTheClone(t *testing.T) {
	f := newFixture(t)
	clone := f.cloneDir(f.demo)
	db := filepath.Join(f.dataDir, "fake-database.db")
	rel, err := filepath.Rel(clone, db)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"db-relative.txt": rel,                                   // ../../../../fake-database.db
		"db-absolute.txt": db,                                    // /tmp/.../fake-database.db
		"up":              filepath.Join("..", "..", "..", ".."), // the data folder
		"app/inside.go":   "refund.go",                           // stays inside: allowed
	} {
		if err := os.Symlink(target, filepath.Join(clone, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	tb := f.toolbox(f.chat)

	refused := []struct{ tool, args string }{
		{"read_file", `{"path": "urtuba/demo/db-relative.txt"}`},
		{"read_file", `{"path": "urtuba/demo/db-absolute.txt"}`},
		{"read_file", `{"path": "urtuba/demo/up/fake-database.db"}`},
		{"read_file", `{"path": "urtuba/demo/` + filepath.ToSlash(rel) + `"}`},
		{"read_file", `{"path": "urtuba/demo/app/../../docs/../../../../../fake-database.db"}`},
		{"read_file", `{"path": "urtuba/demo/` + filepath.ToSlash(db) + `"}`},
		{"read_file", `{"path": "` + filepath.ToSlash(db) + `"}`},
		{"list_files", `{"path": "urtuba/demo/up"}`},
		{"list_files", `{"path": "urtuba/demo/.."}`},
		{"list_files", `{"path": "urtuba/demo/../../../.."}`},
		{"search_text", `{"query": "TOPSECRET", "path": "urtuba/demo/up"}`},
	}
	for _, tt := range refused {
		_, got := call(t, tb, tt.tool, tt.args)
		if !strings.HasPrefix(got, "Error: ") || strings.Contains(got, "TOPSECRET") {
			t.Errorf("%s %s = %q, want an error", tt.tool, tt.args, got)
		}
		// The error never names the server's folders.
		if strings.Contains(got, f.dataDir) && !strings.Contains(tt.args, f.dataDir) {
			t.Errorf("%s %s names a server folder: %q", tt.tool, tt.args, got)
		}
	}
	// A search of everything skips the links.
	if _, got := call(t, tb, "search_text", `{"query": "TOPSECRET"}`); got != `No lines contain "TOPSECRET".` {
		t.Errorf("search of every repository = %q", got)
	}
	// A link that stays inside the clone works.
	if _, got := call(t, tb, "read_file", `{"path": "urtuba/demo/app/inside.go", "from_line": 3, "to_line": 3}`); !strings.Contains(got, "3: // Refund pays") {
		t.Errorf("link inside the clone = %q", got)
	}
	// A ".." that stays inside works too.
	if _, got := call(t, tb, "read_file", `{"path": "urtuba/demo/docs/../app/refund.go", "from_line": 1, "to_line": 1}`); !strings.Contains(got, "1: package app") {
		t.Errorf(".. inside the clone = %q", got)
	}
}

// TestCloneFolderFromDatabase checks that a folder name in the database
// that is not a plain name opens nothing.
func TestCloneFolderFromDatabase(t *testing.T) {
	f := newFixture(t)
	for _, folder := range []string{"..", "../2", "/etc"} {
		if err := f.st.SaveSync(f.ctx, f.demo.ID, folder, commitA, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		agent := must(f.st.Agent(f.ctx, f.project.ID, f.answer.ID))
		tb, skipped, err := f.svc.openRepos(f.ctx, f.project.ID, agent)
		if err != nil {
			t.Fatal(err)
		}
		if len(tb.repos) != 0 || fmt.Sprint(skipped) != "[urtuba/demo]" {
			t.Errorf("folder %q: repos %v, skipped %v", folder, tb.repos, skipped)
		}
		tb.close()
	}
}

func TestTwoBranchesOfOneRepository(t *testing.T) {
	f := newFixture(t)
	dev := must(f.st.AddResource(f.ctx, store.NewResource{
		ProjectID: f.project.ID, Owner: "urtuba", Repo: "demo", Branch: "feature/x",
	}, f.clock.now()))
	dir := filepath.Join(f.dataDir, "repos", fmt.Sprint(f.project.ID), fmt.Sprint(dev.ID), "1")
	os.MkdirAll(dir, 0o750)
	os.WriteFile(filepath.Join(dir, "branch.txt"), []byte("on the branch\n"), 0o600)
	if err := f.st.SaveSync(f.ctx, dev.ID, "1", commitB, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	tb := f.toolbox(f.chat)
	if _, got := call(t, tb, "read_file", `{"path": "urtuba/demo@feature/x/branch.txt"}`); !strings.Contains(got, "1: on the branch") {
		t.Errorf("read on the branch = %q", got)
	}
	if _, got := call(t, tb, "read_file", `{"path": "urtuba/demo/branch.txt"}`); got != "Error: urtuba/demo/branch.txt does not exist." {
		t.Errorf("read on the default branch = %q", got)
	}
}
