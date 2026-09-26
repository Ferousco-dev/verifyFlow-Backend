package user

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pgUniqueViolation = "23505"

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const userColumns = `id::text, full_name, username, email, password_hash,
	email_verified, is_active, role, created_at, updated_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.FullName, &u.Username, &u.Email, &u.PasswordHash,
		&u.EmailVerified, &u.IsActive, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Create inserts a user. Uniqueness is enforced by the database constraints
// (authoritative under concurrency); violations are mapped to sentinel errors.
func (r *Repository) Create(ctx context.Context, u User) (User, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO users (full_name, username, email, password_hash)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+userColumns,
		u.FullName, u.Username, u.Email, u.PasswordHash)
	created, err := scanUser(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			switch pgErr.ConstraintName {
			case "users_email_key":
				return User{}, ErrEmailTaken
			case "users_username_key":
				return User{}, ErrUsernameTaken
			}
		}
		return User{}, err
	}
	return created, nil
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email))
}

func (r *Repository) GetByID(ctx context.Context, id string) (User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1::uuid`, id))
}

func (r *Repository) UpdatePasswordHash(ctx context.Context, id, hash string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1::uuid`, id, hash)
	return err
}

// UpdateRole sets the target user's role. It returns ErrNotFound if no such
// user exists.
func (r *Repository) UpdateRole(ctx context.Context, id, role string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET role = $2, updated_at = now() WHERE id = $1::uuid`, id, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountAdmins returns how many active users currently hold the admin role.
func (r *Repository) CountAdmins(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&count)
	return count, err
}
