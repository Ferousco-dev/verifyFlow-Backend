package providerconfig

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

const configColumns = `id::text, provider_kind, provider_key, config_name, is_enabled,
	credential_key_version, created_at, updated_at`

func scanConfig(row pgx.Row) (Config, error) {
	var c Config
	err := row.Scan(&c.ID, &c.Kind, &c.Key, &c.Name, &c.IsEnabled, &c.CredentialKeyVersion, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Config{}, ErrNotFound
	}
	return c, err
}

func (r *Repository) Create(ctx context.Context, kind, key, name string, ciphertext []byte, keyVersion string) (Config, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO provider_configs (provider_kind, provider_key, config_name, credentials_ciphertext, credential_key_version)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+configColumns,
		kind, key, name, ciphertext, keyVersion,
	)
	created, err := scanConfig(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == "provider_configs_name_key" {
			return Config{}, ErrDuplicateConfig
		}
		return Config{}, err
	}
	return created, nil
}

func (r *Repository) List(ctx context.Context, kind string) ([]Config, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+configColumns+` FROM provider_configs
		  WHERE $1 = '' OR provider_kind = $1
		  ORDER BY provider_kind, provider_key, config_name`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []Config
	for rows.Next() {
		var c Config
		if err := rows.Scan(&c.ID, &c.Kind, &c.Key, &c.Name, &c.IsEnabled, &c.CredentialKeyVersion, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

func (r *Repository) Get(ctx context.Context, id string) (Config, error) {
	return scanConfig(r.pool.QueryRow(ctx, `SELECT `+configColumns+` FROM provider_configs WHERE id = $1::uuid`, id))
}

func (r *Repository) GetCiphertext(ctx context.Context, id string) ([]byte, string, error) {
	var ciphertext []byte
	var keyVersion string
	err := r.pool.QueryRow(ctx,
		`SELECT credentials_ciphertext, credential_key_version FROM provider_configs WHERE id = $1::uuid`, id,
	).Scan(&ciphertext, &keyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return ciphertext, keyVersion, err
}

func (r *Repository) SetEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE provider_configs SET is_enabled = $2, updated_at = now() WHERE id = $1::uuid`, id, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
