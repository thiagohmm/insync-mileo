package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

// Deduplication methods

func (r *SQLiteRepository) SaveDedupKey(ctx context.Context, key *domain.FileDeduplicationKey) error {
	query := `INSERT OR REPLACE INTO deduplication_keys (md5_checksum, size) VALUES (?, ?)`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, key.MD5Checksum, key.Size)
		return err
	})
}

func (r *SQLiteRepository) GetDedupKey(ctx context.Context, md5 string, size int64) (*domain.FileDeduplicationKey, error) {
	query := `SELECT md5_checksum, size FROM deduplication_keys WHERE md5_checksum = ? AND size = ?`
	row := r.db.QueryRowContext(ctx, query, md5, size)
	var key domain.FileDeduplicationKey
	if err := row.Scan(&key.MD5Checksum, &key.Size); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &key, nil
}

func (r *SQLiteRepository) ListFilesByDedupKey(ctx context.Context, key *domain.FileDeduplicationKey) ([]domain.FileMetadata, error) {
	query := `SELECT id, sync_config_id, path, etag, size, last_modified, is_directory, md5_checksum
		FROM file_metadata
		WHERE etag = ? AND size = ?`
	rows, err := r.db.QueryContext(ctx, query, key.MD5Checksum, key.Size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.FileMetadata
	for rows.Next() {
		var m domain.FileMetadata
		var md5Checksum sql.NullString
		if err := rows.Scan(&m.ID, &m.SyncConfigID, &m.Path, &m.ETag, &m.Size, &m.LastModified, &m.IsDirectory, &md5Checksum); err != nil {
			return nil, err
		}
		if md5Checksum.Valid {
			m.MD5Checksum = md5Checksum.String
		}
		list = append(list, m)
	}
	return list, nil
}

// Task queue methods

func (r *SQLiteRepository) SaveSyncTask(ctx context.Context, task *domain.SyncTask) error {
	query := `INSERT INTO sync_tasks (sync_config_id, file_path, remote_id, file_size, task_type, priority, status, created_at, updated_at, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	return r.retryOnBusy(ctx, func() error {
		result, err := r.db.ExecContext(ctx, query, task.SyncConfigID, task.FilePath, task.RemoteID, task.FileSize, task.TaskType, task.Priority, task.Status, task.CreatedAt, task.UpdatedAt, task.Error)
		if err != nil {
			return err
		}
		task.ID, _ = result.LastInsertId()
		return nil
	})
}

func (r *SQLiteRepository) GetNextSyncTask(ctx context.Context, limit int) ([]domain.SyncTask, error) {
	query := `SELECT id, sync_config_id, file_path, remote_id, file_size, task_type, priority, status, created_at, updated_at, error FROM sync_tasks WHERE status = ? ORDER BY priority DESC, created_at ASC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, query, int(domain.TaskPending), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.SyncTask
	for rows.Next() {
		var t domain.SyncTask
		var errorStr sql.NullString
		if err := rows.Scan(&t.ID, &t.SyncConfigID, &t.FilePath, &t.RemoteID, &t.FileSize, &t.TaskType, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt, &errorStr); err != nil {
			return nil, err
		}
		if errorStr.Valid {
			t.Error = errorStr.String
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (r *SQLiteRepository) UpdateSyncTask(ctx context.Context, task *domain.SyncTask) error {
	query := `UPDATE sync_tasks SET status = ?, updated_at = ?, error = ? WHERE id = ?`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, task.Status, task.UpdatedAt, task.Error, task.ID)
		return err
	})
}

func (r *SQLiteRepository) DeleteSyncTask(ctx context.Context, id int64) error {
	query := `DELETE FROM sync_tasks WHERE id = ?`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, id)
		return err
	})
}

func (r *SQLiteRepository) ListSyncTasksByConfig(ctx context.Context, configID int64) ([]domain.SyncTask, error) {
	query := `SELECT id, sync_config_id, file_path, remote_id, file_size, task_type, priority, status, created_at, updated_at, error FROM sync_tasks WHERE sync_config_id = ? ORDER BY priority DESC, created_at ASC`
	rows, err := r.db.QueryContext(ctx, query, configID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.SyncTask
	for rows.Next() {
		var t domain.SyncTask
		var errorStr sql.NullString
		if err := rows.Scan(&t.ID, &t.SyncConfigID, &t.FilePath, &t.RemoteID, &t.FileSize, &t.TaskType, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt, &errorStr); err != nil {
			return nil, err
		}
		if errorStr.Valid {
			t.Error = errorStr.String
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// Webhook methods

func (r *SQLiteRepository) SaveWebhookConfig(ctx context.Context, config *domain.WebhookConfig) error {
	query := `INSERT INTO webhook_configs (sync_config_id, channel_id, resource_id, expiration, event_type) VALUES (?, ?, ?, ?, ?)`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, config.SyncConfigID, config.ChannelID, config.ResourceID, config.Expiration, config.EventType)
		return err
	})
}

func (r *SQLiteRepository) GetWebhookConfig(ctx context.Context, syncConfigID int64) (*domain.WebhookConfig, error) {
	query := `SELECT id, sync_config_id, channel_id, resource_id, expiration, event_type FROM webhook_configs WHERE sync_config_id = ?`
	row := r.db.QueryRowContext(ctx, query, syncConfigID)
	var c domain.WebhookConfig
	if err := row.Scan(&c.ID, &c.SyncConfigID, &c.ChannelID, &c.ResourceID, &c.Expiration, &c.EventType); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

func (r *SQLiteRepository) ListWebhookConfigs(ctx context.Context) ([]domain.WebhookConfig, error) {
	query := `SELECT id, sync_config_id, channel_id, resource_id, expiration, event_type FROM webhook_configs`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []domain.WebhookConfig
	for rows.Next() {
		var c domain.WebhookConfig
		if err := rows.Scan(&c.ID, &c.SyncConfigID, &c.ChannelID, &c.ResourceID, &c.Expiration, &c.EventType); err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	return configs, nil
}

func (r *SQLiteRepository) DeleteWebhookConfig(ctx context.Context, id int64) error {
	query := `DELETE FROM webhook_configs WHERE id = ?`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, id)
		return err
	})
}

// Proxy methods

func (r *SQLiteRepository) SaveProxyConfig(ctx context.Context, config *domain.ProxyConfig) error {
	query := `INSERT OR REPLACE INTO proxy_config (id, host, port, user, password, enabled) VALUES (1, ?, ?, ?, ?, ?)`
	return r.retryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, query, config.Host, config.Port, config.User, config.Password, config.Enabled)
		return err
	})
}

func (r *SQLiteRepository) GetProxyConfig(ctx context.Context) (*domain.ProxyConfig, error) {
	query := `SELECT id, host, port, user, password, enabled FROM proxy_config WHERE id = 1`
	row := r.db.QueryRowContext(ctx, query)
	var c domain.ProxyConfig
	if err := row.Scan(&c.ID, &c.Host, &c.Port, &c.User, &c.Password, &c.Enabled); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// Add new tables for advanced features
func (r *SQLiteRepository) createAdvancedTables() error {
	queries := []string{
		// Deduplication keys table
		`CREATE TABLE IF NOT EXISTS deduplication_keys (
			md5_checksum TEXT PRIMARY KEY,
			size INTEGER NOT NULL,
			first_seen DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,
		// Sync tasks table
		`CREATE TABLE IF NOT EXISTS sync_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sync_config_id INTEGER NOT NULL,
			file_path TEXT NOT NULL,
			remote_id TEXT,
			file_size INTEGER,
			task_type INTEGER NOT NULL,
			priority INTEGER NOT NULL,
			status INTEGER NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			error TEXT,
			FOREIGN KEY (sync_config_id) REFERENCES sync_configs(id)
		);`,
		// Webhook configs table
		`CREATE TABLE IF NOT EXISTS webhook_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sync_config_id INTEGER NOT NULL,
			channel_id TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			expiration DATETIME NOT NULL,
			event_type TEXT NOT NULL,
			FOREIGN KEY (sync_config_id) REFERENCES sync_configs(id)
		);`,
	}

	for _, query := range queries {
		if _, err := r.db.Exec(query); err != nil {
			return fmt.Errorf("failed to create table: %w", err)
		}
	}

	// Create indexes for better performance
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sync_tasks_priority ON sync_tasks(priority);`,
		`CREATE INDEX IF NOT EXISTS idx_sync_tasks_status ON sync_tasks(status);`,
		`CREATE INDEX IF NOT EXISTS idx_sync_tasks_config ON sync_tasks(sync_config_id);`,
		`CREATE INDEX IF NOT EXISTS idx_webhook_configs_sync ON webhook_configs(sync_config_id);`,
		`CREATE INDEX IF NOT EXISTS idx_dedup_keys_size ON deduplication_keys(size);`,
	}

	// Proxy config table (singleton, id=1)
	proxyQuery := `CREATE TABLE IF NOT EXISTS proxy_config (
		id INTEGER PRIMARY KEY,
		host TEXT NOT NULL DEFAULT '',
		port INTEGER NOT NULL DEFAULT 0,
		user TEXT NOT NULL DEFAULT '',
		password TEXT NOT NULL DEFAULT '',
		enabled BOOLEAN NOT NULL DEFAULT 0
	);`
	if _, err := r.db.Exec(proxyQuery); err != nil {
		return fmt.Errorf("failed to create proxy_config table: %w", err)
	}

	for _, query := range indexes {
		if _, err := r.db.Exec(query); err != nil {
			return fmt.Errorf("failed to create index: %w", err)
		}
	}

	return nil
}
