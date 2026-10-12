package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestPasswordReset(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	owner, _ := s.CreateOwner(ctx, "samed", "hash", testNow)
	ayse, _ := s.CreateUser(ctx, "ayse", "old-hash", testNow)
	if err := s.CreateSession(ctx, "session-1", ayse.ID, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	expires := testNow.Add(24 * time.Hour)
	if err := s.CreatePasswordReset(ctx, ayse.ID, "reset-1", owner.ID, testNow, expires); err != nil {
		t.Fatal(err)
	}

	r, err := s.PasswordResetByHash(ctx, "reset-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.UserID != ayse.ID || r.Username != "ayse" || !r.ExpiresAt.Equal(expires) || !r.Open(testNow) {
		t.Errorf("reset = %+v, want an open link for ayse", r)
	}

	accounts, err := s.Accounts(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].Username != "ayse" || !accounts[0].ResetOpenUntil.Equal(expires) || !accounts[1].ResetOpenUntil.IsZero() {
		t.Errorf("Accounts() = %+v, want ayse with an open link, then samed without", accounts)
	}

	id, err := s.UsePasswordReset(ctx, r.ID, "new-hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if id != ayse.ID {
		t.Errorf("user id = %d, want %d", id, ayse.ID)
	}
	if u, _ := s.UserByID(ctx, ayse.ID); u.PasswordHash != "new-hash" {
		t.Errorf("password hash = %q, want new-hash", u.PasswordHash)
	}
	if _, _, err := s.SessionUser(ctx, "session-1", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("old session after reset: error = %v, want ErrNotFound", err)
	}
	if _, err := s.UsePasswordReset(ctx, r.ID, "other-hash", testNow); !errors.Is(err, ErrResetGone) {
		t.Errorf("second use: error = %v, want ErrResetGone", err)
	}
}

func TestNewResetLinkReplacesOld(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	ayse, _ := s.CreateUser(ctx, "ayse", "hash", testNow)
	expires := testNow.Add(time.Hour)
	if err := s.CreatePasswordReset(ctx, ayse.ID, "reset-1", 0, testNow, expires); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePasswordReset(ctx, ayse.ID, "reset-2", 0, testNow, expires); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PasswordResetByHash(ctx, "reset-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old link: error = %v, want ErrNotFound", err)
	}
	if _, err := s.PasswordResetByHash(ctx, "reset-2"); err != nil {
		t.Errorf("new link: %v", err)
	}
}

func TestExpiredResetLink(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	ayse, _ := s.CreateUser(ctx, "ayse", "hash", testNow)
	if err := s.CreatePasswordReset(ctx, ayse.ID, "reset-1", 0, testNow, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r, _ := s.PasswordResetByHash(ctx, "reset-1")
	if _, err := s.UsePasswordReset(ctx, r.ID, "new-hash", testNow.Add(time.Hour)); !errors.Is(err, ErrResetGone) {
		t.Errorf("expired link: error = %v, want ErrResetGone", err)
	}
	if u, _ := s.UserByID(ctx, ayse.ID); u.PasswordHash != "hash" {
		t.Errorf("an expired link changed the password")
	}
}

func TestAccountProjects(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	ayse, _ := s.CreateUser(ctx, "ayse", "hash", testNow)
	bora, _ := s.CreateUser(ctx, "bora", "hash", testNow)
	clinic, _ := s.CreateProject(ctx, "Clinic app", testNow)
	billing, _ := s.CreateProject(ctx, "Billing", testNow)
	if err := s.AddMember(ctx, clinic.ID, ayse.ID, []string{"resources:view", "agents:edit"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, billing.ID, ayse.ID, nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.AccountProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64][]AccountProject{
		ayse.ID: {
			{ProjectID: billing.ID, Name: "Billing", Permissions: []string{}},
			{ProjectID: clinic.ID, Name: "Clinic app", Permissions: []string{"agents:edit", "resources:view"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AccountProjects() = %+v, want %+v", got, want)
	}
	if _, ok := got[bora.ID]; ok {
		t.Error("an account with no projects should not be in the map")
	}
}
