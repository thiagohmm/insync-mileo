package domain

import "context"

type Repository interface {
	SaveAccount(ctx context.Context, account *Account) error
	GetAccount(ctx context.Context, id string) (*Account, error)
	// Conta mais recente para um provedor (ex.: após OAuth no callback HTTP).
	GetLatestAccountByProvider(ctx context.Context, provider Provider) (*Account, error)

	SaveSyncConfig(ctx context.Context, config *SyncConfig) error
	ListSyncConfigs(ctx context.Context) ([]SyncConfig, error)
	GetSyncConfigByPath(ctx context.Context, path string) (*SyncConfig, error)

	UpdateFileMetadata(ctx context.Context, metadata *FileMetadata) error
	GetFileMetadata(ctx context.Context, syncConfigID int64, path string) (*FileMetadata, error)
	DeleteFileMetadata(ctx context.Context, syncConfigID int64, path string) error
	ListFileMetadata(ctx context.Context, syncConfigID int64) ([]FileMetadata, error)
}

type CloudService interface {
	GetProvider() Provider
	ListFiles(ctx context.Context, folderID string) ([]FileMetadata, error)
	UploadFile(ctx context.Context, localPath string, remoteFolderID string) (string, error)
	DownloadFile(ctx context.Context, remoteFileID string, localPath string) error
	DeleteFile(ctx context.Context, remoteFileID string) error
}

type SyncUseCase interface {
	SyncAll(ctx context.Context) error
	SyncFolder(ctx context.Context, config SyncConfig) error
}
