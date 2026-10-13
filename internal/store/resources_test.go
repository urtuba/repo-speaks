package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// resourceFixture is two projects, with no resources yet.
type resourceFixture struct {
	s       *Store
	project Project
	other   Project
}

func newResourceFixture(t *testing.T) resourceFixture {
	t.Helper()
	ctx := context.Background()
	s := openTest(t)
	p, err := s.CreateProject(ctx, "Customer portal", testNow)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateProject(ctx, "Billing", testNow)
	if err != nil {
		t.Fatal(err)
	}
	return resourceFixture{s: s, project: p, other: other}
}

// add adds a repository to the fixture's first project.
func (f resourceFixture) add(t *testing.T, owner, repo, branch, token string) Resource {
	t.Helper()
	r, err := f.s.AddResource(context.Background(), NewResource{
		ProjectID: f.project.ID, Owner: owner, Repo: repo, Branch: branch, Token: token,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const testToken = "github_pat_11AAAAAAA0secretvalue_wxyz"

func TestAddResource(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "", testToken)

	want := Resource{
		ID:        r.ID,
		ProjectID: f.project.ID,
		Kind:      KindGitHubRepo,
		Owner:     "urtuba",
		Repo:      "repo-speaks",
		HasToken:  true,
		TokenHint: "wxyz",
		CreatedAt: testNow,
		OpenJob:   "queued",
	}
	if r != want {
		t.Errorf("AddResource() = %+v, want %+v", r, want)
	}
	got, err := f.s.Resource(ctx, f.project.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Resource() = %+v, want %+v", got, want)
	}

	// Adding queues the first sync, so a second one is not queued.
	if queued, err := f.s.QueueSync(ctx, r.ID, testNow); err != nil || queued {
		t.Errorf("QueueSync after add = %v, %v, want false (already queued)", queued, err)
	}
}

func TestAddResourceRejectsDuplicates(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	f.add(t, "urtuba", "repo-speaks", "", "")
	f.add(t, "urtuba", "repo-speaks", "dev", "")

	tests := []struct {
		name    string
		project int64
		owner   string
		repo    string
		branch  string
		wantErr error
	}{
		{"same default branch", f.project.ID, "urtuba", "repo-speaks", "", ErrResourceExists},
		{"same named branch", f.project.ID, "urtuba", "repo-speaks", "dev", ErrResourceExists},
		{"names differ only in case", f.project.ID, "Urtuba", "Repo-Speaks", "", ErrResourceExists},
		{"branch differs in case", f.project.ID, "urtuba", "repo-speaks", "Dev", nil},
		{"another branch", f.project.ID, "urtuba", "repo-speaks", "main", nil},
		{"another project", f.other.ID, "urtuba", "repo-speaks", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.s.AddResource(ctx, NewResource{
				ProjectID: tt.project, Owner: tt.owner, Repo: tt.repo, Branch: tt.branch,
			}, testNow)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("AddResource() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestResourceIndexTreatsNullBranchAsOne checks the index on its own,
// without the check AddResource makes first: two rows for the default
// branch (NULL) of one repository must clash.
func TestResourceIndexTreatsNullBranchAsOne(t *testing.T) {
	f := newResourceFixture(t)
	insert := func() error {
		_, err := f.s.db.ExecContext(context.Background(), `
			INSERT INTO resources (project_id, kind, owner, repo, branch, created_at)
			VALUES (?, 'github_repo', 'urtuba', 'repo-speaks', NULL, 0)`, f.project.ID)
		return err
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
	if err := insert(); err == nil {
		t.Error("second row for the default branch: error = nil, want a unique constraint error")
	}
}

func TestResources(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	b := f.add(t, "urtuba", "zeta", "", "")
	a := f.add(t, "urtuba", "alpha", "", "")
	c := f.add(t, "urtuba", "alpha", "dev", "")
	if _, err := f.s.AddResource(ctx, NewResource{ProjectID: f.other.ID, Owner: "x", Repo: "y"}, testNow); err != nil {
		t.Fatal(err)
	}

	list, err := f.s.Resources(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range list {
		ids = append(ids, r.ID)
	}
	if want := []int64{a.ID, c.ID, b.ID}; !reflect.DeepEqual(ids, want) {
		t.Errorf("Resources() ids = %v, want %v", ids, want)
	}

	// The open job shows as running once claimed, and goes away when done.
	job, err := f.s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.Resource(ctx, f.project.ID, job.ResourceID); got.OpenJob != "running" {
		t.Errorf("OpenJob = %q, want running", got.OpenJob)
	}
	if err := f.s.FinishJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.Resource(ctx, f.project.ID, job.ResourceID); got.OpenJob != "" {
		t.Errorf("OpenJob after done = %q, want none", got.OpenJob)
	}
}

func TestResourceIsScopedToItsProject(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "", testToken)

	if _, err := f.s.Resource(ctx, f.other.ID, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resource() from another project: error = %v, want ErrNotFound", err)
	}
	if err := f.s.SetResourceToken(ctx, f.other.ID, r.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetResourceToken() from another project: error = %v, want ErrNotFound", err)
	}
	if err := f.s.DeleteResource(ctx, f.other.ID, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteResource() from another project: error = %v, want ErrNotFound", err)
	}
	if ok, err := f.s.ResourceExists(ctx, f.other.ID, r.ID); err != nil || ok {
		t.Errorf("ResourceExists() from another project = %v, %v, want false", ok, err)
	}
	// Nothing changed.
	src, err := f.s.SyncSource(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src.Token != testToken {
		t.Errorf("token = %q, want it unchanged", src.Token)
	}
}

func TestSetResourceToken(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "", "")

	tests := []struct {
		name      string
		token     string
		wantHas   bool
		wantHint  string
		wantToken string
	}{
		{"set", testToken, true, "wxyz", testToken},
		{"replace", "ghp_000000000000000000000000000000000abc", true, "0abc", "ghp_000000000000000000000000000000000abc"},
		{"clear", "", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := f.s.SetResourceToken(ctx, f.project.ID, r.ID, tt.token); err != nil {
				t.Fatal(err)
			}
			got, err := f.s.Resource(ctx, f.project.ID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.HasToken != tt.wantHas || got.TokenHint != tt.wantHint {
				t.Errorf("HasToken, TokenHint = %v, %q, want %v, %q", got.HasToken, got.TokenHint, tt.wantHas, tt.wantHint)
			}
			src, err := f.s.SyncSource(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if src.Token != tt.wantToken {
				t.Errorf("SyncSource().Token = %q, want %q", src.Token, tt.wantToken)
			}
		})
	}

	if err := f.s.SetResourceToken(ctx, f.project.ID, 999, testToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing resource: error = %v, want ErrNotFound", err)
	}
}

func TestTokenHint(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{"", ""},
		{"short", ""},
		{"elevenchars", ""},
		{"twelve_chars", "hars"},
		{testToken, "wxyz"},
	}
	for _, tt := range tests {
		if got := tokenHint(tt.token); got != tt.want {
			t.Errorf("tokenHint(%q) = %q, want %q", tt.token, got, tt.want)
		}
	}
}

func TestSyncSourceAndSaveSync(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "dev", testToken)
	if err := f.s.SetSyncSettings(ctx, f.project.ID, 6, 1<<20); err != nil {
		t.Fatal(err)
	}

	src, err := f.s.SyncSource(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := SyncSource{
		ResourceID:   r.ID,
		ProjectID:    f.project.ID,
		Kind:         KindGitHubRepo,
		Owner:        "urtuba",
		Repo:         "repo-speaks",
		Branch:       "dev",
		Token:        testToken,
		MaxFileBytes: 1 << 20,
	}
	if src != want {
		t.Errorf("SyncSource() = %+v, want %+v", src, want)
	}

	// A failed job leaves an error; a good sync clears it.
	job, err := f.s.ClaimJob(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.FailJob(ctx, job, "GitHub said no"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.Resource(ctx, f.project.ID, r.ID); got.LastError != "GitHub said no" {
		t.Errorf("LastError = %q, want the job's error", got.LastError)
	}

	const commit = "0123456789abcdef0123456789abcdef01234567"
	later := testNow.Add(time.Hour)
	if err := f.s.SaveSync(ctx, r.ID, "42", commit, later); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.Resource(ctx, f.project.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CloneDir != "42" || got.LastCommit != commit || !got.LastSyncedAt.Equal(later) || got.LastError != "" {
		t.Errorf("after SaveSync: %+v", got)
	}
	if src, _ := f.s.SyncSource(ctx, r.ID); src.CloneDir != "42" {
		t.Errorf("SyncSource().CloneDir = %q, want 42", src.CloneDir)
	}

	if err := f.s.SaveSync(ctx, 999, "42", commit, later); !errors.Is(err, ErrNotFound) {
		t.Errorf("SaveSync() for a missing resource: error = %v, want ErrNotFound", err)
	}
	if _, err := f.s.SyncSource(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("SyncSource() for a missing resource: error = %v, want ErrNotFound", err)
	}
}

func TestDeleteResource(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "", "")

	if err := f.s.DeleteResource(ctx, f.project.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Resource(ctx, f.project.ID, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resource() after delete: error = %v, want ErrNotFound", err)
	}
	if ok, _ := f.s.ResourceExists(ctx, f.project.ID, r.ID); ok {
		t.Error("ResourceExists() after delete = true")
	}
	// The queued sync went with it.
	if _, err := f.s.ClaimJob(ctx, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClaimJob() after delete: error = %v, want ErrNotFound", err)
	}
	if err := f.s.DeleteResource(ctx, f.project.ID, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: error = %v, want ErrNotFound", err)
	}
}

func TestDeleteProjectDeletesResources(t *testing.T) {
	ctx := context.Background()
	f := newResourceFixture(t)
	r := f.add(t, "urtuba", "repo-speaks", "", "")
	if err := f.s.DeleteProject(ctx, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SyncSource(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("resource after project delete: error = %v, want ErrNotFound", err)
	}
	if _, err := f.s.ClaimJob(ctx, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("job after project delete: error = %v, want ErrNotFound", err)
	}
}
