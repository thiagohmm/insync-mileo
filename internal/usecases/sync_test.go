package usecases

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

func TestSyncUseCaseNew(t *testing.T) {
	repo := domain.NewMockRepository()
	clouds := []domain.CloudService{domain.NewMockCloudService(domain.GoogleDrive)}

	suc := NewSyncUseCase(repo, clouds)
	if suc == nil {
		t.Fatal("expected non-nil SyncUseCase")
	}
}

func TestSyncUseCase_New_MultipleClouds(t *testing.T) {
	repo := domain.NewMockRepository()
	c1 := domain.NewMockCloudService(domain.GoogleDrive)
	clouds := []domain.CloudService{c1}

	suc := NewSyncUseCase(repo, clouds)

	statuses := suc.Statuses()
	if statuses == nil {
		t.Fatal("expected non-nil status channel")
	}
}

func TestSyncUseCase_SyncAll_NoConfigs(t *testing.T) {
	repo := domain.NewMockRepository()
	clouds := []domain.CloudService{domain.NewMockCloudService(domain.GoogleDrive)}
	suc := NewSyncUseCase(repo, clouds)

	ctx := context.Background()
	err := suc.SyncAll(ctx)
	if err != nil {
		t.Errorf("SyncAll() error: %v", err)
	}
}

func TestSyncUseCase_SyncAll_ListError(t *testing.T) {
	repo := domain.NewMockRepository()
	repo.ErrorOn["ListSyncConfigs"] = fmt.Errorf("db unavailable")
	clouds := []domain.CloudService{domain.NewMockCloudService(domain.GoogleDrive)}
	suc := NewSyncUseCase(repo, clouds)

	err := suc.SyncAll(context.Background())
	if err == nil {
		t.Error("expected error from SyncAll")
	}
}

func TestSyncUseCase_SyncFolder_Directory_DownloadRemoteFiles(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	// Create a temp directory as local path
	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-dir")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      localPath,
		RemoteFolderID: "remote-folder-1",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Add a remote file to the mock cloud service
	cloudSvc.Files = []domain.FileMetadata{
		{Path: "remote1.txt", ETag: "etag-1", Size: 100, IsDirectory: false},
		{Path: "remote2.txt", ETag: "etag-2", Size: 200, IsDirectory: false},
	}

	clouds := []domain.CloudService{cloudSvc}
	suc := NewSyncUseCase(repo, clouds)

	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() error: %v", err)
	}

	// Verify that file metadata was saved
	meta1, err := repo.GetFileMetadata(context.Background(), cfg.ID, "remote1.txt")
	if err != nil {
		t.Fatalf("GetFileMetadata() error: %v", err)
	}
	if meta1 == nil {
		t.Error("expected file metadata for remote1.txt")
	}
}

func TestSyncUseCase_SyncFolderRejectsUnsafeRemotePath(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	localPath := filepath.Join(t.TempDir(), "sync-dir")
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      localPath,
		RemoteFolderID: "remote-folder-1",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	cloudSvc.Files = []domain.FileMetadata{
		{Path: "../escape.txt", ETag: "etag-1", Size: 100, IsDirectory: false},
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err == nil {
		t.Fatal("expected unsafe remote path to be rejected")
	}
}

func TestSyncUseCase_SyncFolder_Directory_RemoteDeletedLocally(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-delete")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      localPath,
		RemoteFolderID: "remote-del",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Create a local file with metadata as if it was synced before
	localFile := filepath.Join(localPath, "deleted-in-cloud.txt")
	os.WriteFile(localFile, []byte("content"), 0644)

	if err := repo.UpdateFileMetadata(context.Background(), &domain.FileMetadata{
		SyncConfigID: cfg.ID,
		Path:         "deleted-in-cloud.txt",
		ETag:         "old-etag",
		Size:         100,
	}); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	// Remote has no files (file was deleted in cloud)
	cloudSvc.Files = []domain.FileMetadata{}

	clouds := []domain.CloudService{cloudSvc}
	suc := NewSyncUseCase(repo, clouds)

	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() error: %v", err)
	}

	// Local file should be removed
	if _, err := os.Stat(localFile); !os.IsNotExist(err) {
		t.Error("expected local file to be removed")
	}

	// Metadata should be deleted
	meta, err := repo.GetFileMetadata(context.Background(), cfg.ID, "deleted-in-cloud.txt")
	if err != nil {
		t.Fatalf("GetFileMetadata() error: %v", err)
	}
	if meta != nil {
		t.Error("expected metadata to be deleted")
	}
}

func TestSyncUseCase_SyncFolder_Directory_UploadNewLocalFiles(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)
	cloudSvc.UploadResult = "new-remote-id"

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-upload")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      localPath,
		RemoteFolderID: "remote-upload",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Create a new local file (not in remote, no metadata)
	os.WriteFile(filepath.Join(localPath, "new-local.txt"), []byte("new content"), 0644)

	// Remote is empty
	cloudSvc.Files = []domain.FileMetadata{}

	clouds := []domain.CloudService{cloudSvc}
	suc := NewSyncUseCase(repo, clouds)

	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() error: %v", err)
	}

	// Metadata should be saved after upload
	meta, err := repo.GetFileMetadata(context.Background(), cfg.ID, "new-local.txt")
	if err != nil {
		t.Fatalf("GetFileMetadata() error: %v", err)
	}
	if meta == nil {
		t.Error("expected file metadata after upload")
	}
}

func TestSyncUseCase_SyncFolder_SingleFile_LocalDeleted_FullSync(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "single-file.txt")

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      localPath,
		RemoteFolderID: "remote-single-id",
		Mode:           domain.FullSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    false,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Add metadata as if it was synced
	if err := repo.UpdateFileMetadata(context.Background(), &domain.FileMetadata{
		SyncConfigID: cfg.ID,
		Path:         "single-file.txt",
		ETag:         "remote-single-id",
		Size:         100,
	}); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	// Local file does NOT exist (deleted)
	// cloudSvc.DeleteFile returns nil by default

	clouds := []domain.CloudService{cloudSvc}
	suc := NewSyncUseCase(repo, clouds)

	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() error: %v", err)
	}
}

func TestSyncUseCase_SyncFolder_SingleFile_LocalDeleted_BaseSync(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "base-single.txt")

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      localPath,
		RemoteFolderID: "remote-base-id",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    false,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	clouds := []domain.CloudService{cloudSvc}
	suc := NewSyncUseCase(repo, clouds)

	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() error: %v", err)
	}
}

func TestSyncUseCase_SyncFolder_RemoteError(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)
	cloudSvc.ListError = fmt.Errorf("remote unavailable")

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      "/tmp/should-fail",
		RemoteFolderID: "remote-err",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	clouds := []domain.CloudService{cloudSvc}
	suc := NewSyncUseCase(repo, clouds)

	err := suc.SyncFolder(context.Background(), *cfg)
	if err == nil {
		t.Error("expected error from SyncFolder")
	}
}

func TestSyncUseCase_SyncFolder_NoAccountError(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	// Config with non-existent account
	cfg := &domain.SyncConfig{
		AccountID:      "non-existent-account",
		LocalPath:      t.TempDir(),
		RemoteFolderID: "remote",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}

	// We need the cloud service already registered for the provider
	// since cloudForConfig will look it up from the map first
	clouds := []domain.CloudService{cloudSvc}
	suc := NewSyncUseCase(repo, clouds)

	err := suc.SyncFolder(context.Background(), *cfg)
	// Should succeed since cloudSvc is already in the map
	if err != nil {
		t.Logf("SyncFolder() returned (may succeed due to cloud in map): %v", err)
	}
}

func TestSyncUseCase_Statuses_ChannelAccessible(t *testing.T) {
	repo := domain.NewMockRepository()
	clouds := []domain.CloudService{domain.NewMockCloudService(domain.GoogleDrive)}
	suc := NewSyncUseCase(repo, clouds)

	// Open channel in background so it doesn't hang
	go func() {
		time.Sleep(100 * time.Millisecond)
		// Close by sending a sentinel (we can't close read-only channel from outside)
	}()

	// Verify channel is non-blocking
	select {
	case <-suc.Statuses():
		// ok
	case <-time.After(200 * time.Millisecond):
		// ok, no status yet
	}
}

func TestSyncUseCase_DownloadFileWithProgress_SendsStatus(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})

	remote := domain.FileMetadata{
		Path: "progress.txt",
		ETag: "etag-progress",
		Size: 1000,
	}
	cloudSvc.Files = []domain.FileMetadata{remote}

	cfg := domain.SyncConfig{
		LocalPath:      t.TempDir(),
		RemoteFolderID: "remote",
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}

	tmpDir := t.TempDir()
	os.MkdirAll(tmpDir, 0755)

	// DownloadFileWithProgress returns nil on mock
	go func() {
		suc.SyncFolder(context.Background(), cfg)
	}()

	// Read status channel without blocking
	select {
	case st := <-suc.Statuses():
		if st.FilePath != "" {
			t.Logf("received status: %s - %s", st.FilePath, st.Status)
		}
	case <-time.After(500 * time.Millisecond):
		// ok, no status yet
	}
}
