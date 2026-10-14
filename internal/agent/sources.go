package agent

import (
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/urtuba/repo-speaks/internal/store"
)

// maxSources caps the sources of one answer.
const maxSources = 30

// citationTail matches what follows a repository's label in a citation:
// "/path:from-to" or "/path:line". A path ends at a space, a colon or a
// sign that wraps text in Markdown. The dash may be a hyphen or the
// longer dash some models write.
const citationTail = `/([^\s:` + "`" + `'"<>()\[\]{}|,;*]+):(\d+)(?:\s*[-\x{2013}]\s*(\d+))?`

// sources finds the citations in an answer and keeps those of files read
// during the answer, with a sane line range, in the order they first
// appear. Each source gets the commit its clone had at question start.
func (t *toolbox) sources(answer string) []store.NewSource {
	type found struct {
		at  int
		src store.NewSource
	}
	var all []found
	for _, r := range t.repos {
		// The label must not be the end of a longer name, so
		// "xurtuba/demo" is no citation of urtuba/demo.
		re := regexp.MustCompile(`(?i)(?:^|[^\w./@-])` + regexp.QuoteMeta(r.label) + citationTail)
		for _, m := range re.FindAllStringSubmatchIndex(answer, -1) {
			p := path.Clean(answer[m[2]:m[3]])
			from, err1 := strconv.Atoi(answer[m[4]:m[5]])
			to, err2 := from, error(nil)
			if m[6] >= 0 {
				to, err2 = strconv.Atoi(answer[m[6]:m[7]])
			}
			lines, read := t.read[fileKey{r.resourceID, p}]
			if err1 != nil || err2 != nil || !read || !fs.ValidPath(p) || from < 1 || to < from || to > lines {
				continue
			}
			all = append(all, found{m[0], store.NewSource{
				ResourceID: r.resourceID, Path: p, LineFrom: from, LineTo: to, CommitID: r.commit,
			}})
		}
	}
	slices.SortStableFunc(all, func(a, b found) int { return a.at - b.at })
	var out []store.NewSource
	for _, f := range all {
		if len(out) == maxSources {
			break
		}
		if !slices.Contains(out, f.src) {
			out = append(out, f.src)
		}
	}
	return out
}

// commitRE matches a full commit ID: SHA-1, or SHA-256.
var commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// SourceURL returns the GitHub link of a source: the file at the commit,
// with the line range. It returns "" when the commit is not a full
// commit ID. Only the app builds these links; the model's text never
// becomes one.
func SourceURL(src store.Source) string {
	if !commitRE.MatchString(src.CommitID) || src.Owner == "" || src.Repo == "" || src.LineFrom < 1 {
		return ""
	}
	segments := strings.Split(src.Path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	u := "https://github.com/" + url.PathEscape(src.Owner) + "/" + url.PathEscape(src.Repo) +
		"/blob/" + src.CommitID + "/" + strings.Join(segments, "/") + "#L" + strconv.Itoa(src.LineFrom)
	if src.LineTo > src.LineFrom {
		u += "-L" + strconv.Itoa(src.LineTo)
	}
	return u
}
