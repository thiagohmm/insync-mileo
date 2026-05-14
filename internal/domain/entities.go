package domain

import "time"

type Provider string

const (
	GoogleDrive Provider = "GOOGLE_DRIVE"
	OneDrive    Provider = "ONEDRIVE"
)

type SyncMode int

const (
	BaseSync SyncMode = iota // Cloud -> Local (Deletions local don't affect Cloud)
	FullSync                 // Bidirectional
)

type Account struct {
	ID           string
	Provider     Provider
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

type SyncConfig struct {
	ID             int64
	AccountID      string
	LocalPath      string
	RemoteFolderID string
	Mode           SyncMode
	Provider       Provider
}

type FileMetadata struct {
	ID           int64
	SyncConfigID int64
	Path         string
	ETag         string
	Size         int64
	LastModified time.Time
	IsDirectory  bool
}

type SyncStatus struct {
	FilePath           string
	Status             string
	ProgressPercentage int32
	TotalSize          int64
	ProcessedSize      int64
	Provider           Provider
}
