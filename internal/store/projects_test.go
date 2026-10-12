package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestProjects(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	ayse, err := s.CreateUser(ctx, "ayse", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	portal, err := s.CreateProject(ctx, "Customer portal", testNow)
	if err != nil {
		t.Fatal(err)
	}
	billing, err := s.CreateProject(ctx, "Billing", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, portal.ID, ayse.ID, nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.Project(ctx, portal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != portal {
		t.Errorf("Project() = %+v, want %+v", got, portal)
	}
	if _, err := s.Project(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing project: error = %v, want ErrNotFound", err)
	}

	all, err := s.AllProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Project{billing, portal}; !reflect.DeepEqual(all, want) {
		t.Errorf("AllProjects() = %+v, want %+v", all, want)
	}

	mine, err := s.UserProjects(ctx, ayse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Project{portal}; !reflect.DeepEqual(mine, want) {
		t.Errorf("UserProjects() = %+v, want %+v", mine, want)
	}
}

func TestMembers(t *testing.T) {
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
	ayse, _ := s.CreateUser(ctx, "ayse", "hash", testNow)
	mehmet, _ := s.CreateUser(ctx, "mehmet", "hash", testNow)
	zeynep, _ := s.CreateUser(ctx, "zeynep", "hash", testNow)

	adds := []struct {
		project int64
		user    int64
		perms   []string
	}{
		{p.ID, mehmet.ID, []string{"resources:view", "members:view"}},
		{p.ID, ayse.ID, nil},
		{other.ID, zeynep.ID, []string{"resources:view"}},
	}
	for _, a := range adds {
		if err := s.AddMember(ctx, a.project, a.user, a.perms); err != nil {
			t.Fatal(err)
		}
	}

	members, err := s.Members(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []Member{
		{UserID: ayse.ID, Username: "ayse", Permissions: []string{}},
		{UserID: mehmet.ID, Username: "mehmet", Permissions: []string{"members:view", "resources:view"}},
	}
	if !reflect.DeepEqual(members, want) {
		t.Errorf("Members() = %+v, want %+v", members, want)
	}

	tests := []struct {
		name    string
		user    int64
		want    []string
		wantErr error
	}{
		{"member with permissions", mehmet.ID, []string{"members:view", "resources:view"}, nil},
		{"member with none", ayse.ID, []string{}, nil},
		{"member of another project", zeynep.ID, nil, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.MemberPermissions(ctx, p.ID, tt.user)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("MemberPermissions() error = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MemberPermissions() = %v, want %v", got, tt.want)
			}
		})
	}

	if err := s.AddMember(ctx, p.ID, ayse.ID, nil); err == nil {
		t.Error("adding a member twice: no error")
	}
}

func TestAddMemberRejectsUnknownPermission(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u, err := s.CreateUser(ctx, "ayse", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, "Customer portal", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, p.ID, u.ID, []string{"resources:view", "agents:use"}); err == nil {
		t.Fatal("AddMember with agents:use: error = nil, want a foreign key error")
	}
	// The membership must not be half made.
	if _, err := s.MemberPermissions(ctx, p.ID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after a failed AddMember: error = %v, want ErrNotFound", err)
	}
}

func TestSetMemberPermissions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u, _ := s.CreateUser(ctx, "ayse", "hash", testNow)
	p, _ := s.CreateProject(ctx, "Customer portal", testNow)
	if err := s.AddMember(ctx, p.ID, u.ID, []string{"resources:view"}); err != nil {
		t.Fatal(err)
	}

	want := []string{"agents:edit", "resources:view"}
	if err := s.SetMemberPermissions(ctx, p.ID, u.ID, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.MemberPermissions(ctx, p.ID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("permissions = %v, want %v", got, want)
	}

	// An unknown name fails, and the old permissions stay.
	if err := s.SetMemberPermissions(ctx, p.ID, u.ID, []string{"agents:use"}); err == nil {
		t.Error("unknown permission: error = nil")
	}
	if got, _ := s.MemberPermissions(ctx, p.ID, u.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("after a failed change: permissions = %v, want %v", got, want)
	}

	if err := s.SetMemberPermissions(ctx, p.ID, 999, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("not a member: error = %v, want ErrNotFound", err)
	}
}

func TestRenameAndDeleteProject(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u, _ := s.CreateUser(ctx, "ayse", "hash", testNow)
	p, _ := s.CreateProject(ctx, "Customer portal", testNow)
	if err := s.AddMember(ctx, p.ID, u.ID, []string{"resources:view"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInvite(ctx, p.ID, "hash-1", "asker", u.ID, testNow, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameProject(ctx, p.ID, "Portal"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Project(ctx, p.ID); got.Name != "Portal" {
		t.Errorf("name = %q, want Portal", got.Name)
	}
	if err := s.RenameProject(ctx, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing project: error = %v, want ErrNotFound", err)
	}

	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("project after delete: error = %v, want ErrNotFound", err)
	}
	if _, err := s.MemberPermissions(ctx, p.ID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("membership after delete: error = %v, want ErrNotFound", err)
	}
	if _, err := s.InviteByHash(ctx, "hash-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("invite after delete: error = %v, want ErrNotFound", err)
	}
	if _, err := s.UserByID(ctx, u.ID); err != nil {
		t.Errorf("the account must stay: %v", err)
	}
}
