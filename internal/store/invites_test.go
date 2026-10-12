package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// inviteFixture is a project with a manager who has made one invite.
type inviteFixture struct {
	s       *Store
	project Project
	manager User
	invite  Invite
}

func newInviteFixture(t *testing.T) inviteFixture {
	t.Helper()
	ctx := context.Background()
	s := openTest(t)
	manager, err := s.CreateUser(ctx, "mert", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, "Customer portal", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, p.ID, manager.ID, []string{"members:edit"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInvite(ctx, p.ID, "hash-1", "asker", manager.ID, testNow, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	inv, err := s.InviteByHash(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	return inviteFixture{s: s, project: p, manager: manager, invite: inv}
}

func TestInviteByHash(t *testing.T) {
	f := newInviteFixture(t)
	want := Invite{
		ID:            f.invite.ID,
		ProjectID:     f.project.ID,
		ProjectName:   "Customer portal",
		Preset:        "asker",
		CreatedBy:     f.manager.ID,
		CreatedByName: "mert",
		CreatedAt:     testNow,
		ExpiresAt:     testNow.Add(time.Hour),
	}
	if !reflect.DeepEqual(f.invite, want) {
		t.Errorf("InviteByHash() = %+v, want %+v", f.invite, want)
	}
	if !f.invite.Open(testNow) || f.invite.Open(testNow.Add(time.Hour)) {
		t.Error("Open() must be true before the expiry and false at it")
	}
	if _, err := f.s.InviteByHash(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash: error = %v, want ErrNotFound", err)
	}
}

func TestAcceptInviteNewAccount(t *testing.T) {
	f := newInviteFixture(t)
	ctx := context.Background()
	id, err := f.s.AcceptInvite(ctx, Acceptance{
		Invite: f.invite, Permissions: []string{"resources:view"}, Now: testNow,
		Username: "ayse", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	perms, err := f.s.MemberPermissions(ctx, f.project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"resources:view"}; !reflect.DeepEqual(perms, want) {
		t.Errorf("permissions = %v, want %v", perms, want)
	}
	inv, err := f.s.InviteByHash(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Used {
		t.Error("invite is not marked used")
	}

	// The same link a second time.
	_, err = f.s.AcceptInvite(ctx, Acceptance{
		Invite: f.invite, Now: testNow, Username: "zeynep", PasswordHash: "hash",
	})
	if !errors.Is(err, ErrInviteGone) {
		t.Errorf("second use: error = %v, want ErrInviteGone", err)
	}
	if _, err := f.s.UserByUsername(ctx, "zeynep"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a failed accept made an account: error = %v, want ErrNotFound", err)
	}
}

func TestAcceptInviteFailuresChangeNothing(t *testing.T) {
	tests := []struct {
		name    string
		accept  func(f inviteFixture) Acceptance
		prepare func(t *testing.T, f inviteFixture)
		wantErr error
	}{
		{
			name: "username taken",
			accept: func(f inviteFixture) Acceptance {
				return Acceptance{Invite: f.invite, Now: testNow, Username: "mert", PasswordHash: "hash"}
			},
			wantErr: ErrUsernameTaken,
		},
		{
			name: "already a member",
			accept: func(f inviteFixture) Acceptance {
				return Acceptance{Invite: f.invite, Now: testNow, UserID: f.manager.ID}
			},
			wantErr: ErrAlreadyMember,
		},
		{
			name: "expired",
			accept: func(f inviteFixture) Acceptance {
				return Acceptance{Invite: f.invite, Now: testNow.Add(time.Hour), Username: "ayse", PasswordHash: "hash"}
			},
			wantErr: ErrInviteGone,
		},
		{
			name: "revoked",
			prepare: func(t *testing.T, f inviteFixture) {
				if err := f.s.RevokeInvite(context.Background(), f.project.ID, f.invite.ID, testNow); err != nil {
					t.Fatal(err)
				}
			},
			accept: func(f inviteFixture) Acceptance {
				return Acceptance{Invite: f.invite, Now: testNow, Username: "ayse", PasswordHash: "hash"}
			},
			wantErr: ErrInviteGone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			ctx := context.Background()
			if tt.prepare != nil {
				tt.prepare(t, f)
			}
			if _, err := f.s.AcceptInvite(ctx, tt.accept(f)); !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if _, err := f.s.UserByUsername(ctx, "ayse"); !errors.Is(err, ErrNotFound) {
				t.Errorf("a failed accept made an account: error = %v", err)
			}
			inv, err := f.s.InviteByHash(ctx, "hash-1")
			if err != nil {
				t.Fatal(err)
			}
			if inv.Used {
				t.Error("a failed accept marked the invite used")
			}
		})
	}
}

func TestOpenInvitesAndRevoke(t *testing.T) {
	f := newInviteFixture(t)
	ctx := context.Background()
	if err := f.s.CreateInvite(ctx, f.project.ID, "hash-2", "builder", f.manager.ID, testNow.Add(time.Minute), testNow.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	open, err := f.s.OpenInvites(ctx, f.project.ID, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].Preset != "builder" {
		t.Fatalf("OpenInvites() = %+v, want 2 invites, newest (builder) first", open)
	}

	if err := f.s.RevokeInvite(ctx, f.project.ID, f.invite.ID, testNow); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RevokeInvite(ctx, f.project.ID, f.invite.ID, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking twice: error = %v, want ErrNotFound", err)
	}
	if err := f.s.RevokeInvite(ctx, f.project.ID+1, open[0].ID, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking from another project: error = %v, want ErrNotFound", err)
	}

	// Past the first invite's expiry, only the second is open.
	open, err = f.s.OpenInvites(ctx, f.project.ID, testNow.Add(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Preset != "builder" {
		t.Errorf("OpenInvites() later = %+v, want only the builder invite", open)
	}
}

func TestInviteSurvivesCreatorDeletion(t *testing.T) {
	// The store keeps the invite; the web layer refuses it because its
	// creator is gone. Here we check the row survives with CreatedBy 0.
	f := newInviteFixture(t)
	ctx := context.Background()
	if err := f.s.DeleteUser(ctx, f.manager.ID); err != nil {
		t.Fatal(err)
	}
	inv, err := f.s.InviteByHash(ctx, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if inv.CreatedBy != 0 || inv.CreatedByName != "" {
		t.Errorf("creator = %d %q, want 0 and empty", inv.CreatedBy, inv.CreatedByName)
	}
}
