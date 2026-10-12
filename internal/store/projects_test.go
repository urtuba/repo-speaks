package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestProjects(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	ayse, err := s.CreateUser(ctx, "ayse", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	clinic, err := s.CreateProject(ctx, "Clinic app", testNow)
	if err != nil {
		t.Fatal(err)
	}
	billing, err := s.CreateProject(ctx, "Billing", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, clinic.ID, ayse.ID, nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.Project(ctx, clinic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != clinic {
		t.Errorf("Project() = %+v, want %+v", got, clinic)
	}
	if _, err := s.Project(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing project: error = %v, want ErrNotFound", err)
	}

	all, err := s.AllProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Project{billing, clinic}; !reflect.DeepEqual(all, want) {
		t.Errorf("AllProjects() = %+v, want %+v", all, want)
	}

	mine, err := s.UserProjects(ctx, ayse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Project{clinic}; !reflect.DeepEqual(mine, want) {
		t.Errorf("UserProjects() = %+v, want %+v", mine, want)
	}
}

func TestMembers(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p, err := s.CreateProject(ctx, "Clinic app", testNow)
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
	p, err := s.CreateProject(ctx, "Clinic app", testNow)
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
