package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/thiagohmm/insync-clone/internal/domain"
	_ "modernc.org/sqlite"
)

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(dbPath string) (*SQLiteRepository, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	repo := &SQLiteRepository{db: db}
	if err := repo.createTables(); err != nil {
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	return repo, nil
}

func (r *SQLiteRepository) createTables() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			access_token TEXT,
			refresh_token TEXT,
			expiry DATETIME
		);`,
		`CREATE TABLE IF NOT EXISTS sync_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL,
			local_path TEXT NOT NULL UNIQUE,
			remote_folder_id TEXT NOT NULL,
			mode INTEGER NOT NULL,
			provider TEXT NOT NULL,
			is_directory BOOLEAN NOT NULL DEFAULT 1,
			FOREIGN KEY (account_id) REFERENCES accounts(id)
		);`,
		`CREATE TABLE IF NOT EXISTS file_metadata (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sync_config_id INTEGER NOT NULL,
			path TEXT NOT NULL,
			etag TEXT,
			size INTEGER,
			last_modified DATETIME,
			is_directory BOOLEAN,
			FOREIGN KEY (sync_config_id) REFERENCES sync_configs(id),
			UNIQUE(sync_config_id, path)
		);`,
	}

	for _, query := range queries {
		if _, err := r.db.Exec(query); err != nil {
			return err
		}
	}
	_, _ = r.db.Exec(`ALTER TABLE sync_configs ADD COLUMN is_directory BOOLEAN NOT NULL DEFAULT 1`)
	return nil
}

func (r *SQLiteRepository) SaveAccount(ctx context.Context, account *domain.Account) error {
	query := `INSERT OR REPLACE INTO accounts (id, provider, access_token, refresh_token, expiry) VALUES (?, ?, ?, ?, ?)`
	_, err := r.db.ExecContext(ctx, query, account.ID, string(account.Provider), account.AccessToken, account.RefreshToken, account.Expiry)
	return err
}

func (r *SQLiteRepository) GetAccount(ctx context.Context, id string) (*domain.Account, error) {
	query := `SELECT id, provider, access_token, refresh_token, expiry FROM accounts WHERE id = ?`
	row := r.db.QueryRowContext(ctx, query, id)
	var acc domain.Account
	var provider string
	if err := row.Scan(&acc.ID, &provider, &acc.AccessToken, &acc.RefreshToken, &acc.Expiry); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	acc.Provider = domain.Provider(provider)
	return &acc, nil
}

func (r *SQLiteRepository) GetLatestAccountByProvider(ctx context.Context, provider domain.Provider) (*domain.Account, error) {
	query := `SELECT id, provider, access_token, refresh_token, expiry FROM accounts WHERE provider = ? ORDER BY rowid DESC LIMIT 1`
	row := r.db.QueryRowContext(ctx, query, string(provider))
	var acc domain.Account
	var p string
	if err := row.Scan(&acc.ID, &p, &acc.AccessToken, &acc.RefreshToken, &acc.Expiry); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	acc.Provider = domain.Provider(p)
	return &acc, nil
}

func (r *SQLiteRepository) SaveSyncConfig(ctx context.Context, config *domain.SyncConfig) error {
	query := `INSERT INTO sync_configs (account_id, local_path, remote_folder_id, mode, provider, is_directory)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(local_path) DO UPDATE SET
			account_id = excluded.account_id,
			remote_folder_id = excluded.remote_folder_id,
			mode = excluded.mode,
			provider = excluded.provider,
			is_directory = excluded.is_directory`
	result, err := r.db.ExecContext(ctx, query, config.AccountID, config.LocalPath, config.RemoteFolderID, int(config.Mode), string(config.Provider), config.IsDirectory)
	if err != nil {
		return err
	}
	config.ID, _ = result.LastInsertId()
	if config.ID == 0 {
		saved, err := r.GetSyncConfigByPath(ctx, config.LocalPath)
		if err != nil {
			return err
		}
		if saved != nil {
			config.ID = saved.ID
		}
	}
	return nil
}

func (r *SQLiteRepository) ListSyncConfigs(ctx context.Context) ([]domain.SyncConfig, error) {
	query := `SELECT id, account_id, local_path, remote_folder_id, mode, provider, is_directory FROM sync_configs`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []domain.SyncConfig
	for rows.Next() {
		var c domain.SyncConfig
		var provider string
		if err := rows.Scan(&c.ID, &c.AccountID, &c.LocalPath, &c.RemoteFolderID, (*int)(&c.Mode), &provider, &c.IsDirectory); err != nil {
			return nil, err
		}
		c.Provider = domain.Provider(provider)
		configs = append(configs, c)
	}
	return configs, nil
}

func (r *SQLiteRepository) GetSyncConfigByPath(ctx context.Context, path string) (*domain.SyncConfig, error) {
	query := `SELECT id, account_id, local_path, remote_folder_id, mode, provider, is_directory FROM sync_configs WHERE local_path = ?`
	row := r.db.QueryRowContext(ctx, query, path)
	var c domain.SyncConfig
	var provider string
	if err := row.Scan(&c.ID, &c.AccountID, &c.LocalPath, &c.RemoteFolderID, (*int)(&c.Mode), &provider, &c.IsDirectory); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	c.Provider = domain.Provider(provider)
	return &c, nil
}

func (r *SQLiteRepository) UpdateFileMetadata(ctx context.Context, metadata *domain.FileMetadata) error {
	query := `INSERT OR REPLACE INTO file_metadata (sync_config_id, path, etag, size, last_modified, is_directory) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := r.db.ExecContext(ctx, query, metadata.SyncConfigID, metadata.Path, metadata.ETag, metadata.Size, metadata.LastModified, metadata.IsDirectory)
	return err
}

func (r *SQLiteRepository) GetFileMetadata(ctx context.Context, syncConfigID int64, path string) (*domain.FileMetadata, error) {
	query := `SELECT id, sync_config_id, path, etag, size, last_modified, is_directory FROM file_metadata WHERE sync_config_id = ? AND path = ?`
	row := r.db.QueryRowContext(ctx, query, syncConfigID, path)
	var m domain.FileMetadata
	if err := row.Scan(&m.ID, &m.SyncConfigID, &m.Path, &m.ETag, &m.Size, &m.LastModified, &m.IsDirectory); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *SQLiteRepository) DeleteFileMetadata(ctx context.Context, syncConfigID int64, path string) error {
	query := `DELETE FROM file_metadata WHERE sync_config_id = ? AND path = ?`
	_, err := r.db.ExecContext(ctx, query, syncConfigID, path)
	return err
}

func (r *SQLiteRepository) ListFileMetadata(ctx context.Context, syncConfigID int64) ([]domain.FileMetadata, error) {
	query := `SELECT id, sync_config_id, path, etag, size, last_modified, is_directory FROM file_metadata WHERE sync_config_id = ?`
	rows, err := r.db.QueryContext(ctx, query, syncConfigID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.FileMetadata
	for rows.Next() {
		var m domain.FileMetadata
		if err := rows.Scan(&m.ID, &m.SyncConfigID, &m.Path, &m.ETag, &m.Size, &m.LastModified, &m.IsDirectory); err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, nil
}

func (r *SQLiteRepository) Close() error {
	return r.db.Close()
}
