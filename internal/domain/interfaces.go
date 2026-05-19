package domain

import "context"

type Repository interface {
	SaveAccount(ctx context.Context, account *Account) error
	GetAccount(ctx context.Context, id string) (*Account, error)
	// Conta mais recente para um provedor (ex.: após OAuth no callback HTTP).
	GetLatestAccountByProvider(ctx context.Context, provider Provider) (*Account, error)
	DeleteAccount(ctx context.Context, id string) error

	SaveSyncConfig(ctx context.Context, config *SyncConfig) error
	ListSyncConfigs(ctx context.Context) ([]SyncConfig, error)
	GetSyncConfigByPath(ctx context.Context, path string) (*SyncConfig, error)
	DeleteSyncConfigByRemoteID(ctx context.Context, accountID, remoteFolderID string) error

	UpdateFileMetadata(ctx context.Context, metadata *FileMetadata) error
	GetFileMetadata(ctx context.Context, syncConfigID int64, path string) (*FileMetadata, error)
	DeleteFileMetadata(ctx context.Context, syncConfigID int64, path string) error
	ListFileMetadata(ctx context.Context, syncConfigID int64) ([]FileMetadata, error)

	// Deduplication methods
	SaveDedupKey(ctx context.Context, key *FileDeduplicationKey) error
	GetDedupKey(ctx context.Context, md5 string, size int64) (*FileDeduplicationKey, error)
	ListFilesByDedupKey(ctx context.Context, key *FileDeduplicationKey) ([]FileMetadata, error)

	// Task queue methods
	SaveSyncTask(ctx context.Context, task *SyncTask) error
	GetNextSyncTask(ctx context.Context, limit int) ([]SyncTask, error)
	UpdateSyncTask(ctx context.Context, task *SyncTask) error
	DeleteSyncTask(ctx context.Context, id int64) error
	ListSyncTasksByConfig(ctx context.Context, configID int64) ([]SyncTask, error)

	// Webhook methods
	SaveWebhookConfig(ctx context.Context, config *WebhookConfig) error
	GetWebhookConfig(ctx context.Context, syncConfigID int64) (*WebhookConfig, error)
	ListWebhookConfigs(ctx context.Context) ([]WebhookConfig, error)
	DeleteWebhookConfig(ctx context.Context, id int64) error

	// Proxy methods
	SaveProxyConfig(ctx context.Context, config *ProxyConfig) error
	GetProxyConfig(ctx context.Context) (*ProxyConfig, error)
}

type CloudService interface {
	GetProvider() Provider
	ListFiles(ctx context.Context, folderID string) ([]FileMetadata, error)
	UploadFile(ctx context.Context, localPath string, remoteFolderID string) (string, error)
	DownloadFile(ctx context.Context, remoteFileID string, localPath string) error
	DownloadFileWithProgress(ctx context.Context, remoteFileID string, localPath string, onProgress func(downloaded, total int64)) error
	DeleteFile(ctx context.Context, remoteFileID string) error
	// Checksum and metadata methods
	GetFileChecksum(ctx context.Context, remoteFileID string) (string, error)
	GetFileMimeType(ctx context.Context, remoteFileID string) (string, error)
}

type WebhookHandler interface {
	HandleNotification(notification *PushNotification) error
	RegisterWebhook(ctx context.Context, config *WebhookConfig) error
	UnregisterWebhook(ctx context.Context, config *WebhookConfig) error
	StartCallbackServer(port int) error
}

type TaskQueue interface {
	Enqueue(task *SyncTask) error
	Dequeue() (*SyncTask, error)
	GetNextTasks(limit int) ([]SyncTask, error)
	UpdateTask(task *SyncTask) error
	DeleteTask(id int64) error
	CountByPriority(priority SyncPriority) (int, error)
}

type DeduplicationService interface {
	ComputeChecksum(filePath string) (string, error)
	StoreDedupKey(ctx context.Context, key *FileDeduplicationKey) error
	GetExistingFile(ctx context.Context, key *FileDeduplicationKey) (*FileMetadata, error)
	IsDuplicate(ctx context.Context, key *FileDeduplicationKey) (bool, error)
}

type SyncUseCase interface {
	SyncAll(ctx context.Context) error
	SyncFolder(ctx context.Context, config SyncConfig) error
	Statuses() <-chan SyncStatus
	// New methods for advanced features
	ProcessTaskQueue(ctx context.Context) error
	HandleWebhookNotification(notification *PushNotification) error
	RegisterWebhook(ctx context.Context, config SyncConfig) error
	ComputeAndStoreChecksum(ctx context.Context, filePath string) (string, error)
}
