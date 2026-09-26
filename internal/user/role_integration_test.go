package user

import (
	"context"
	"errors"
	"testing"
)

func TestRepositoryDefaultsToUserRole(t *testing.T) {
	repo := newRepo(t)
	created, err := repo.Create(context.Background(), User{FullName: "R", Username: "role-default", Email: "role-default@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Role != RoleUser {
		t.Fatalf("default role = %q, want %q", created.Role, RoleUser)
	}
}

func TestRepositoryUpdateRolePersistsAndCounts(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	u, err := repo.Create(ctx, User{FullName: "A", Username: "role-admin", Email: "role-admin@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if count, err := repo.CountAdmins(ctx); err != nil || count != 0 {
		t.Fatalf("CountAdmins before promotion = %d, %v", count, err)
	}
	if err := repo.UpdateRole(ctx, u.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(ctx, u.ID)
	if err != nil || got.Role != RoleAdmin {
		t.Fatalf("GetByID after promotion: %+v, %v", got, err)
	}
	if count, err := repo.CountAdmins(ctx); err != nil || count != 1 {
		t.Fatalf("CountAdmins after promotion = %d, %v", count, err)
	}
}

func TestRepositoryUpdateRoleRejectsUnknownRole(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	u, err := repo.Create(ctx, User{FullName: "A", Username: "role-invalid", Email: "role-invalid@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateRole(ctx, u.ID, "superuser"); err == nil {
		t.Fatal("expected DB check constraint to reject an unknown role")
	}
}

func TestRepositoryUpdateRoleUnknownUserReturnsNotFound(t *testing.T) {
	repo := newRepo(t)
	err := repo.UpdateRole(context.Background(), "00000000-0000-0000-0000-000000000000", RoleAdmin)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateRole on missing user error = %v", err)
	}
}
