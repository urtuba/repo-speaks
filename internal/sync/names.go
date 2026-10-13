package sync

import (
	"regexp"
	"strings"
)

// The names a sync accepts. They follow GitHub's rules, and none may
// start with "-", so git cannot read one as an option. The web package
// checks the same rules when a builder adds a repository; the sync checks
// again because it must not trust a database row it is about to put in a
// command.
var (
	ownerRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoRE   = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,99}$`)
	branchRE = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,254}$`)
)

// checkNames checks a repository's owner, name and branch. The branch
// may be empty, for the default branch.
func checkNames(owner, repo, branch string) error {
	if !ownerRE.MatchString(owner) {
		return permanentf("%q is not a GitHub owner name", owner)
	}
	if !repoRE.MatchString(repo) || repo == "." || strings.Contains(repo, "..") {
		return permanentf("%q is not a GitHub repository name", repo)
	}
	if branch != "" && (!branchRE.MatchString(branch) || strings.Contains(branch, "..")) {
		return permanentf("%q is not a branch name this app accepts", branch)
	}
	return nil
}
