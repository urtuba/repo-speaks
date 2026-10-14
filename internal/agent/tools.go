package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/urtuba/repo-speaks/internal/llm"
)

// Caps on what one tool call returns, so one step cannot fill the
// model's context.
const (
	maxListEntries  = 300     // entries list_files shows
	maxHits         = 50      // lines search_text returns
	maxHitLen       = 200     // characters of a line search_text shows
	maxReadLines    = 400     // lines read_file returns per call
	maxLineLen      = 500     // characters of a line read_file shows
	maxOutputBytes  = 60_000  // bytes of any tool's result
	maxReadBytes    = 2 << 20 // the largest file read_file or search_text opens
	maxSearchFiles  = 20_000  // files one search looks at
	maxQueryLen     = 200     // characters of a search query
	binarySniffSize = 8000    // bytes checked for a NUL, the sign of a binary file
)

// toolError is a problem the model caused, such as a path that does not
// exist. The model gets its message as the tool's result.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

func toolErrorf(format string, args ...any) error {
	return &toolError{fmt.Sprintf(format, args...)}
}

// repo is a clone the agent may read, opened at question start.
type repo struct {
	// label starts every path of the repository: owner/repo, or
	// owner/repo@branch when the agent reads two branches of it.
	label      string
	resourceID int64
	commit     string
	// root is the clone's folder. Every file access goes through it, so
	// no path or symlink can lead outside the clone.
	root *os.Root
}

// fileKey names a file of a resource.
type fileKey struct {
	resourceID int64
	path       string // clean, relative to the clone
}

// toolbox runs the tools of one answer over the agent's repositories.
type toolbox struct {
	repos []*repo
	// read holds the files read during the answer, with their line
	// counts. Only these may become sources.
	read map[fileKey]int
}

// close closes every repository.
func (t *toolbox) close() {
	for _, r := range t.repos {
		r.root.Close()
	}
}

// toolDefs are the tools every agent gets. They only read.
var toolDefs = []llm.Tool{
	{
		Name: "list_files",
		Description: "Lists the files and folders in a folder of a repository. " +
			"Folders end with a slash.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The folder, as owner/repo or owner/repo/folder.",
				},
			},
			"required": []string{"path"},
		},
	},
	{
		Name: "search_text",
		Description: "Searches the text of the files for a word or phrase, without case. " +
			"A * matches any text inside one line. Returns at most 50 lines, as path:line: text.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The text to find, such as cancel or refund*days.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Optional: search only this repository or folder, as owner/repo or owner/repo/folder.",
				},
			},
			"required": []string{"query"},
		},
	},
	{
		Name: "read_file",
		Description: "Reads lines of a text file, with line numbers. " +
			"Returns at most 400 lines per call; read the rest with another call.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The file, as owner/repo/path.",
				},
				"from_line": map[string]any{
					"type":        "integer",
					"description": "The first line to read. Default 1.",
				},
				"to_line": map[string]any{
					"type":        "integer",
					"description": "The last line to read. Default from_line + 399.",
				},
			},
			"required": []string{"path"},
		},
	},
}

// run runs one tool call. It returns a short line for the asker's page,
// such as "Reading owner/repo/app.go", and the result for the model.
// A problem the model caused becomes the result; other errors, such as
// the end of ctx, are returned.
func (t *toolbox) run(ctx context.Context, call llm.ToolCall) (step, result string, err error) {
	var args struct {
		Path     string `json:"path"`
		Query    string `json:"query"`
		FromLine int    `json:"from_line"`
		ToLine   int    `json:"to_line"`
	}
	argErr := json.Unmarshal([]byte(orEmpty(call.Arguments)), &args)
	switch call.Name {
	case "list_files":
		step = "Listing " + args.Path
		if argErr == nil {
			result, err = t.listFiles(args.Path)
		}
	case "search_text":
		step = "Searching for " + args.Query
		if args.Path != "" {
			step += " in " + args.Path
		}
		if argErr == nil {
			result, err = t.searchText(ctx, args.Query, args.Path)
		}
	case "read_file":
		step = "Reading " + args.Path
		if argErr == nil {
			result, err = t.readFile(args.Path, args.FromLine, args.ToLine)
		}
	default:
		return "Calling an unknown tool", fmt.Sprintf("Error: there is no tool named %q.", call.Name), nil
	}
	if argErr != nil {
		return oneLine(step), "Error: the arguments are not valid JSON.", nil
	}
	if te, ok := errors.AsType[*toolError](err); ok {
		return oneLine(step), "Error: " + te.msg, nil
	}
	if err != nil {
		return "", "", err
	}
	return oneLine(step), capOutput(result), nil
}

// orEmpty returns "{}" for empty arguments, which some models send for a
// call without any.
func orEmpty(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

// oneLine makes a step safe and short for a page: one line, no control
// characters, at most 120 characters.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 120 {
		s = string([]rune(s)[:120]) + "..."
	}
	return s
}

// capOutput cuts a result at maxOutputBytes, on a line break.
func capOutput(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	cut := strings.LastIndexByte(s[:maxOutputBytes], '\n')
	if cut < 0 {
		cut = maxOutputBytes
	}
	return strings.ToValidUTF8(s[:cut], "") + "\n[Output cut. Ask for less, such as fewer lines.]"
}

// resolve finds the repository a path starts with, and returns the rest
// of the path, relative to the clone. Only the agent's repositories are
// known, so a path of any other resource fails here. The rest is not
// cleaned: os.Root checks it, and refuses any ".." that leaves the
// clone.
func (t *toolbox) resolve(p string) (*repo, string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	var found *repo
	for _, r := range t.repos {
		n := len(r.label)
		if len(p) < n || !strings.EqualFold(p[:n], r.label) || (len(p) > n && p[n] != '/') {
			continue
		}
		if found == nil || n > len(found.label) {
			found = r // the longest label wins: owner/repo@dev over owner/repo
		}
	}
	if found == nil {
		return nil, "", toolErrorf("%q is in none of your repositories. Paths start with one of: %s.", p, t.labels())
	}
	rel := strings.TrimPrefix(p[len(found.label):], "/")
	if rel == "" {
		rel = "."
	}
	return found, rel, nil
}

// labels lists the repositories for an error message.
func (t *toolbox) labels() string {
	if len(t.repos) == 0 {
		return "none, since you have no repositories"
	}
	var names []string
	for _, r := range t.repos {
		names = append(names, r.label)
	}
	return strings.Join(names, ", ")
}

// display is how a path shows in results: the label and the clean path.
func (r *repo) display(rel string) string {
	if rel = path.Clean(rel); rel == "." {
		return r.label
	}
	return r.label + "/" + rel
}

// openError explains why a path could not be opened. It never passes on
// err's text, which holds the server's own folder names.
func openError(shown string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return toolErrorf("%s does not exist.", shown)
	}
	return toolErrorf("%s cannot be opened. It may lead outside the repository.", shown)
}

// listFiles lists a folder.
func (t *toolbox) listFiles(p string) (string, error) {
	r, rel, err := t.resolve(p)
	if err != nil {
		return "", err
	}
	shown := r.display(rel)
	f, err := r.root.Open(rel)
	if err != nil {
		return "", openError(shown, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", openError(shown, err)
	}
	if !info.IsDir() {
		return "", toolErrorf("%s is a file. Use read_file to read it.", shown)
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return "", openError(shown, err)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	var b strings.Builder
	fmt.Fprintf(&b, "Contents of %s/:\n", shown)
	for i, e := range entries {
		if i == maxListEntries {
			fmt.Fprintf(&b, "[%d more not shown]\n", len(entries)-maxListEntries)
			break
		}
		b.WriteString(e.Name())
		if e.IsDir() {
			b.WriteString("/")
		}
		b.WriteString("\n")
	}
	if len(entries) == 0 {
		b.WriteString("(empty)\n")
	}
	return b.String(), nil
}

// searchText looks for query in the text files of one folder, or of
// every repository when p is empty.
func (t *toolbox) searchText(ctx context.Context, query, p string) (string, error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) > maxQueryLen {
		return "", toolErrorf("the query is over %d characters.", maxQueryLen)
	}
	parts := patternParts(query)
	if len(parts) == 0 {
		return "", toolErrorf("the query is empty.")
	}
	type place struct {
		r   *repo
		dir string // a valid fs.FS path
	}
	var places []place
	if strings.TrimSpace(p) == "" {
		for _, r := range t.repos {
			places = append(places, place{r, "."})
		}
	} else {
		r, rel, err := t.resolve(p)
		if err != nil {
			return "", err
		}
		dir := path.Clean(rel)
		if !fs.ValidPath(dir) {
			return "", toolErrorf("%s/%s leads outside the repository.", r.label, rel)
		}
		places = append(places, place{r, dir})
	}

	var hits []string
	files, full := 0, false
	for _, pl := range places {
		fsys := pl.r.root.FS()
		err := fs.WalkDir(fsys, pl.dir, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				if name == pl.dir {
					return openError(pl.r.display(name), err)
				}
				return nil // an unreadable folder inside is skipped
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			// Symlinks are skipped: WalkDir does not follow them, and
			// only regular files are read.
			if !d.Type().IsRegular() {
				return nil
			}
			if files++; files > maxSearchFiles {
				full = true
				return fs.SkipAll
			}
			data, ok := readText(fsys, name)
			if !ok {
				return nil
			}
			for i, line := range splitLines(data) {
				if !matches(strings.ToLower(line), parts) {
					continue
				}
				if len(hits) == maxHits {
					full = true
					return fs.SkipAll
				}
				hits = append(hits, fmt.Sprintf("%s:%d: %s", pl.r.display(name), i+1, cutLine(strings.TrimSpace(line), maxHitLen)))
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if full {
			break
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No lines contain %q.", query), nil
	}
	out := strings.Join(hits, "\n") + "\n"
	if full {
		out += "[The search stopped at its limit. Search a folder, or use a longer query.]\n"
	}
	return out, nil
}

// patternParts splits a query at each *, in lower case, without empty
// parts.
func patternParts(query string) []string {
	var parts []string
	for _, part := range strings.Split(strings.ToLower(query), "*") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// matches reports whether line holds every part, in order.
func matches(line string, parts []string) bool {
	for _, part := range parts {
		i := strings.Index(line, part)
		if i < 0 {
			return false
		}
		line = line[i+len(part):]
	}
	return true
}

// readText reads a file through fsys. It returns false for a file too
// large, binary, not UTF-8, or not readable.
func readText(fsys fs.FS, name string) (string, bool) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil || len(data) > maxReadBytes || isBinary(data) || !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

// isBinary reports whether data looks like a binary file: it has a NUL
// byte near the start, as git checks.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniffSize)], 0) >= 0
}

// splitLines splits text into lines, without the line ends. A last line
// break does not start an empty line.
func splitLines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// cutLine cuts a line at n characters.
func cutLine(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + " [line cut]"
}

// readFile reads lines from..to of a file, at most maxReadLines. It
// records the file as read, so the answer may cite it.
func (t *toolbox) readFile(p string, from, to int) (string, error) {
	r, rel, err := t.resolve(p)
	if err != nil {
		return "", err
	}
	shown := r.display(rel)
	f, err := r.root.Open(rel)
	if err != nil {
		return "", openError(shown, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", openError(shown, err)
	}
	if info.IsDir() {
		return "", toolErrorf("%s is a folder. Use list_files to see what is in it.", shown)
	}
	if !info.Mode().IsRegular() {
		return "", toolErrorf("%s is not a regular file.", shown)
	}
	if info.Size() > maxReadBytes {
		return "", toolErrorf("%s is over %d MB, too large to read.", shown, maxReadBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return "", openError(shown, err)
	}
	if len(data) > maxReadBytes {
		return "", toolErrorf("%s is over %d MB, too large to read.", shown, maxReadBytes>>20)
	}
	if isBinary(data) {
		return "", toolErrorf("%s is a binary file, not text, so it cannot be read.", shown)
	}
	if !utf8.Valid(data) {
		return "", toolErrorf("%s is not UTF-8 text, so it cannot be read.", shown)
	}
	lines := splitLines(string(data))
	t.read[fileKey{r.resourceID, path.Clean(rel)}] = len(lines)
	return numbered(shown, lines, from, to)
}

// numbered returns lines from..to with their numbers, and a header. A
// from of 0 means 1; a to of 0 means as many as one call returns.
func numbered(shown string, lines []string, from, to int) (string, error) {
	if from <= 0 {
		from = 1
	}
	if to <= 0 {
		to = from + maxReadLines - 1
	}
	if to < from {
		return "", toolErrorf("to_line %d is before from_line %d.", to, from)
	}
	if len(lines) == 0 {
		return shown + " is empty.\n", nil
	}
	if from > len(lines) {
		return "", toolErrorf("%s has only %d lines.", shown, len(lines))
	}
	note := ""
	if to-from+1 > maxReadLines {
		to = from + maxReadLines - 1
		note = "[At most " + strconv.Itoa(maxReadLines) + " lines per call. Read on from line " + strconv.Itoa(to+1) + ".]\n"
	}
	to = min(to, len(lines))
	var b strings.Builder
	fmt.Fprintf(&b, "%s, lines %d to %d of %d:\n", shown, from, to, len(lines))
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i, cutLine(lines[i-1], maxLineLen))
	}
	b.WriteString(note)
	return b.String(), nil
}
