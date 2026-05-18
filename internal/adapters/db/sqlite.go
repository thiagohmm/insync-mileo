package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
	_ "modernc.org/sqlite"
)

type SQLiteRepository struct {
	db *sql.DB
}

// retryOnBusy executa uma operação com retry exponencial backoff em caso de SQLITE_BUSY
func (r *SQLiteRepository) retryOnBusy(ctx context.Context, op func() error) error {
	maxRetries := 5
	baseDelay := 50 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		err := op()
		if err == nil {
			return nil
		}

		// Verificar se é erro de banco ocupado
		errMsg := err.Error()
		if !strings.Contains(errMsg, "SQLITE_BUSY") && !strings.Contains(errMsg, "database is locked") {
			return err // Erro diferente, retornar imediatamente
		}

		// Última tentativa, retornar o erro
		if attempt == maxRetries-1 {
			return fmt.Errorf("database busy after %d retries: %w", maxRetries, err)
		}

		// Backoff exponencial com jitter
		delay := baseDelay * time.Duration(1<<uint(attempt))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			// Continuar para próxima tentativa
		}
	}

	return fmt.Errorf("max retries exceeded")
}

func NewSQLiteRepository(dbPath string) (*SQLiteRepository, error) {
	if info, err := os.Lstat(dbPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to use symlinked database path: %s", dbPath)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to inspect database path: %w", err)
	}
	dbFile, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create secure database file: %w", err)
	}
	if err := dbFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close database file: %w", err)
	}

	// Adicionar parâmetros para melhor concorrência
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000&_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to secure database permissions: %w", err)
	}

	// Configurar pool de conexões para evitar contenção
	db.SetMaxOpenConns(1) // SQLite funciona melhor com uma única conexão de escrita
	db.SetMaxIdleConns(1)

	repo := &SQLiteRepository{db: db}
	if err := repo.createTables(); err != nil {
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		return nil, fmt.Errorf("failed to secure database permissions: %w", err)
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
			md5_checksum TEXT DEFAULT '',
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
	_, _ = r.db.Exec(`ALTER TABLE file_metadata ADD COLUMN md5_checksum TEXT DEFAULT ''`)
	return nil
}

func (r *SQLiteRepository) SaveAccount(ctx context.Context, account *domain.Account) error {
	query := `INSERT OR REPLACE INTO accounts (id, provider, access_token, refresh_token, expiry) VALUES (?, ?, ?, ?, ?)`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, account.ID, string(account.Provider), account.AccessToken, account.RefreshToken, account.Expiry)
		return err
	})
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

func (r *SQLiteRepository) DeleteAccount(ctx context.Context, id string) error {
	query := `DELETE FROM accounts WHERE id = ?`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, id)
		return err
	})
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

	// Usar retry para operação de escrita
	return r.retryOnBusy(ctx, func() error {
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
	})
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

func (r *SQLiteRepository) DeleteSyncConfigByRemoteID(ctx context.Context, accountID, remoteFolderID string) error {
	// Primeiro deletar os metadados de arquivos associados
	// Para isso, precisamos dos IDs dos sync_configs que correspondem
	query := `DELETE FROM file_metadata WHERE sync_config_id IN (
		SELECT id FROM sync_configs WHERE account_id = ? AND remote_folder_id = ?
	)`
	if err := r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, accountID, remoteFolderID)
		return err
	}); err != nil {
		return err
	}

	// Depois deletar o sync_config em si
	query = `DELETE FROM sync_configs WHERE account_id = ? AND remote_folder_id = ?`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, accountID, remoteFolderID)
		return err
	})
}

func (r *SQLiteRepository) UpdateFileMetadata(ctx context.Context, metadata *domain.FileMetadata) error {
	query := `INSERT OR REPLACE INTO file_metadata (sync_config_id, path, etag, size, last_modified, is_directory, md5_checksum) VALUES (?, ?, ?, ?, ?, ?, ?)`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, metadata.SyncConfigID, metadata.Path, metadata.ETag, metadata.Size, metadata.LastModified, metadata.IsDirectory, metadata.MD5Checksum)
		return err
	})
}

func (r *SQLiteRepository) GetFileMetadata(ctx context.Context, syncConfigID int64, path string) (*domain.FileMetadata, error) {
	query := `SELECT id, sync_config_id, path, etag, size, last_modified, is_directory, md5_checksum FROM file_metadata WHERE sync_config_id = ? AND path = ?`
	row := r.db.QueryRowContext(ctx, query, syncConfigID, path)
	var m domain.FileMetadata
	var md5 sql.NullString
	if err := row.Scan(&m.ID, &m.SyncConfigID, &m.Path, &m.ETag, &m.Size, &m.LastModified, &m.IsDirectory, &md5); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if md5.Valid {
		m.MD5Checksum = md5.String
	}
	return &m, nil
}

func (r *SQLiteRepository) DeleteFileMetadata(ctx context.Context, syncConfigID int64, path string) error {
	query := `DELETE FROM file_metadata WHERE sync_config_id = ? AND path = ?`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, syncConfigID, path)
		return err
	})
}

func (r *SQLiteRepository) ListFileMetadata(ctx context.Context, syncConfigID int64) ([]domain.FileMetadata, error) {
	query := `SELECT id, sync_config_id, path, etag, size, last_modified, is_directory, md5_checksum FROM file_metadata WHERE sync_config_id = ?`
	rows, err := r.db.QueryContext(ctx, query, syncConfigID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.FileMetadata
	for rows.Next() {
		var m domain.FileMetadata
		var md5 sql.NullString
		if err := rows.Scan(&m.ID, &m.SyncConfigID, &m.Path, &m.ETag, &m.Size, &m.LastModified, &m.IsDirectory, &md5); err != nil {
			return nil, err
		}
		if md5.Valid {
			m.MD5Checksum = md5.String
		}
		list = append(list, m)
	}
	return list, nil
}

func (r *SQLiteRepository) Close() error {
	return r.db.Close()
}
