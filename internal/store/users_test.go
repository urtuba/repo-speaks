package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func TestCreateOwnerOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if has, err := s.HasUsers(ctx); err != nil || has {
		t.Fatalf("HasUsers() = %v, %v, want false, nil", has, err)
	}
	owner, err := s.CreateOwner(ctx, "samed", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !owner.IsOwner || owner.ID == 0 {
		t.Errorf("CreateOwner() = %+v, want an owner with an id", owner)
	}
	if has, err := s.HasUsers(ctx); err != nil || !has {
		t.Fatalf("HasUsers() = %v, %v, want true, nil", has, err)
	}
	if _, err := s.CreateOwner(ctx, "other", "hash", testNow); !errors.Is(err, ErrSetupDone) {
		t.Errorf("second CreateOwner() error = %v, want ErrSetupDone", err)
	}
}

func TestUserByUsername(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	created, err := s.CreateUser(ctx, "ayse", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.UserByUsername(ctx, "ayse")
	if err != nil {
		t.Fatal(err)
	}
	if got != created {
		t.Errorf("UserByUsername() = %+v, want %+v", got, created)
	}
	if got.IsOwner {
		t.Error("CreateUser made an owner")
	}
	if _, err := s.UserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown username: error = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateUser(ctx, "ayse", "hash", testNow); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("same username twice: error = %v, want ErrUsernameTaken", err)
	}
}

func TestSetPasswordEndsSessions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u, err := s.CreateUser(ctx, "ayse", "old", testNow)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "mehmet", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	expires := testNow.Add(time.Hour)
	for _, sess := range []struct {
		hash   string
		userID int64
	}{{"a1", u.ID}, {"a2", u.ID}, {"m1", other.ID}} {
		if err := s.CreateSession(ctx, sess.hash, sess.userID, expires); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.SetPassword(ctx, u.ID, "new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByUsername(ctx, "ayse"); got.PasswordHash != "new" {
		t.Errorf("password hash = %q, want %q", got.PasswordHash, "new")
	}
	for _, hash := range []string{"a1", "a2"} {
		if _, _, err := s.SessionUser(ctx, hash, testNow); !errors.Is(err, ErrNotFound) {
			t.Errorf("session %s after SetPassword: error = %v, want ErrNotFound", hash, err)
		}
	}
	if _, _, err := s.SessionUser(ctx, "m1", testNow); err != nil {
		t.Errorf("another account's session ended: %v", err)
	}
	if err := s.SetPassword(ctx, 999, "new"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPassword for a missing user: error = %v, want ErrNotFound", err)
	}
}

func TestDeleteUserEndsSessionsAndMemberships(t *testing.T) {
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
	if err := s.AddMember(ctx, p.ID, u.ID, []string{"resources:view"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "a1", u.ID, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, "a1", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("session after DeleteUser: error = %v, want ErrNotFound", err)
	}
	if members, err := s.Members(ctx, p.ID); err != nil || len(members) != 0 {
		t.Errorf("Members() = %v, %v, want none", members, err)
	}
}

func TestOwner(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if _, err := s.Owner(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("before setup: error = %v, want ErrNotFound", err)
	}
	owner, err := s.CreateOwner(ctx, "samed", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "ayse", "hash", testNow); err != nil {
		t.Fatal(err)
	}
	got, err := s.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != owner {
		t.Errorf("Owner() = %+v, want %+v", got, owner)
	}
}
