package domain

import (
	"database/sql/driver"
	"fmt"
	"time"
)

// NullableTime is a time.Time that can be null in the database.
// It marshals/unmarshals as RFC3339 strings for reliable SQLite persistence.
type NullableTime struct {
	Time  time.Time
	Valid bool
}

func (nt *NullableTime) Scan(value interface{}) error {
	if value == nil {
		nt.Time = time.Time{}
		nt.Valid = false
		return nil
	}
	switch v := value.(type) {
	case time.Time:
		nt.Time = v
		nt.Valid = true
	case []byte:
		t, err := time.Parse(time.RFC3339, string(v))
		if err != nil {
			return fmt.Errorf("NullableTime.Scan: failed to parse time: %w", err)
		}
		nt.Time = t
		nt.Valid = true
	case string:
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fmt.Errorf("NullableTime.Scan: failed to parse time: %w", err)
		}
		nt.Time = t
		nt.Valid = true
	default:
		return fmt.Errorf("NullableTime.Scan: unsupported type %T", value)
	}
	return nil
}

func (nt NullableTime) Value() (driver.Value, error) {
	if !nt.Valid {
		return nil, nil
	}
	return nt.Time.UTC().Format(time.RFC3339), nil
}

type Provider string

const (
	GoogleDrive Provider = "GOOGLE_DRIVE"
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
	Expiry       NullableTime
}

type SyncConfig struct {
	ID             int64
	AccountID      string
	LocalPath      string
	RemoteFolderID string
	Mode           SyncMode
	Provider       Provider
	IsDirectory    bool
}

type FileMetadata struct {
	ID           int64
	SyncConfigID int64
	Path         string
	ETag         string
	Size         int64
	LastModified time.Time
	IsDirectory  bool
	// MD5Checksum is the hex-encoded MD5 hash reported by the cloud provider.
	// Used to verify download integrity.
	MD5Checksum string
}

// FileDeduplicationKey represents a key for deduplicating files by content
type FileDeduplicationKey struct {
	MD5Checksum string
	Size        int64
}

type SyncPriority int

const (
	LowPriority      SyncPriority = iota // 0
	NormalPriority                       // 1
	HighPriority                         // 2
	CriticalPriority                     // 3
)

type SyncTask struct {
	ID           int64
	SyncConfigID int64
	FilePath     string
	RemoteID     string
	FileSize     int64
	TaskType     SyncTaskType
	Priority     SyncPriority
	Status       SyncTaskStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Error        string
}

type SyncTaskType int

const (
	DownloadTask SyncTaskType = iota
	UploadTask
	DeleteLocalTask
	DeleteRemoteTask
	VerifyChecksumTask
)

type SyncTaskStatus int

const (
	TaskPending SyncTaskStatus = iota
	TaskRunning
	TaskCompleted
	TaskFailed
	TaskCancelled
)

// WebhookConfig represents configuration for cloud webhooks
type WebhookConfig struct {
	ID           int64
	SyncConfigID int64
	ChannelID    string
	ResourceID   string
	Expiration   time.Time
	EventType    string // e.g., "push_notification", "webhook"
}

// PushNotification represents a webhook notification from cloud provider
type PushNotification struct {
	ResourceID string
	ChannelID  string
	Expiration time.Time
	Changed    string // Changed resource ID (file/folder ID)
	State      string // Current state of the resource
	AuthError  bool   // Whether authentication error occurred
}

type SyncStatus struct {
	FilePath           string
	Status             string
	ProgressPercentage int32
	TotalSize          int64
	ProcessedSize      int64
	Provider           Provider
}
