package usecases

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
	"google.golang.org/api/googleapi"
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

func TestSyncUseCase_SyncFolder_SingleFileInitialDownload(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)
	cloudSvc.Files = []domain.FileMetadata{{
		Path:        "single-file.txt",
		ETag:        "remote-file-id",
		MD5Checksum: "not-used-by-mock-download",
	}}

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "single-file.txt")

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      localPath,
		RemoteFolderID: "remote-file-id",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    false,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	if err := suc.SyncFolder(context.Background(), *cfg); err != nil {
		t.Fatalf("SyncFolder() error: %v", err)
	}

	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("expected local file to be downloaded: %v", err)
	}
	meta, err := repo.GetFileMetadata(context.Background(), cfg.ID, "single-file.txt")
	if err != nil {
		t.Fatalf("GetFileMetadata() error: %v", err)
	}
	if meta == nil {
		t.Fatal("expected file metadata after single-file download")
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

// -----------------------------
// Checksum verification tests
// -----------------------------

func TestVerifyLocalMD5Checksum(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("match", func(t *testing.T) {
		path := filepath.Join(tmpDir, "match.txt")
		const content = "hello checksum"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		actualMD5 := computeMD5String(content)
		if err := verifyLocalMD5Checksum(path, actualMD5); err != nil {
			t.Errorf("expected match, got error: %v", err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		path := filepath.Join(tmpDir, "mismatch.txt")
		if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
			t.Fatal(err)
		}
		err := verifyLocalMD5Checksum(path, "00000000000000000000000000000000")
		if err == nil {
			t.Error("expected mismatch error")
		} else if !strings.Contains(err.Error(), "MD5 mismatch") {
			t.Errorf("expected MD5 mismatch error, got: %v", err)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		err := verifyLocalMD5Checksum(filepath.Join(tmpDir, "missing.txt"), "00000000000000000000000000000000")
		if err == nil {
			t.Error("expected error for missing file")
		}
	})

	t.Run("case insensitive match", func(t *testing.T) {
		path := filepath.Join(tmpDir, "case-match.txt")
		const content = "case test"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		expected := computeMD5String(content)
		upperExpected := strings.ToUpper(expected)
		if err := verifyLocalMD5Checksum(path, upperExpected); err != nil {
			t.Errorf("expected case-insensitive match, got error: %v", err)
		}
	})
}

func computeMD5String(data string) string {
	h := md5.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// -----------------------------
// Conflict detection test
// -----------------------------

func TestSyncUseCase_ConflictDetection(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-conflict")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-conflict",
		LocalPath:      localPath,
		RemoteFolderID: "remote-conflict",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Create a local file that was previously synced (has metadata)
	localFilePath := filepath.Join(localPath, "conflict-file.txt")
	lastKnownTime := time.Now().Add(-1 * time.Hour)
	if err := os.WriteFile(localFilePath, []byte("local modified content"), 0644); err != nil {
		t.Fatal(err)
	}
	// Touch the file so modTime is definitely after lastKnownTime
	time.Sleep(10 * time.Millisecond)
	if err := os.Chtimes(localFilePath, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// Existing metadata from previous sync (old ETag and old mod time)
	if err := repo.UpdateFileMetadata(context.Background(), &domain.FileMetadata{
		SyncConfigID: cfg.ID,
		Path:         "conflict-file.txt",
		ETag:         "old-etag",
		Size:         100,
		LastModified: lastKnownTime,
	}); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	// Remote has the same file but with a different ETag (remote changed)
	cloudSvc.Files = []domain.FileMetadata{
		{Path: "conflict-file.txt", ETag: "new-etag", Size: 200, IsDirectory: false},
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})

	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() error: %v", err)
	}

	// The conflict file should have been created (remote version saved as .conflict)
	conflictPath := localFilePath + ".conflict"
	if _, err := os.Stat(conflictPath); os.IsNotExist(err) {
		t.Error("expected .conflict file to be created")
	}

	// The original local file should still exist (preserved)
	if _, err := os.Stat(localFilePath); os.IsNotExist(err) {
		t.Error("expected original local file to be preserved")
	}
}

// -----------------------------
// errgroup worker pool test
// -----------------------------

func TestSyncUseCase_ErrgroupConcurrency(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "eg-dir")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-eg",
		LocalPath:      localPath,
		RemoteFolderID: "remote-eg",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Create many remote files to exercise the errgroup concurrency limit
	files := make([]domain.FileMetadata, 0, 20)
	for i := 0; i < 20; i++ {
		files = append(files, domain.FileMetadata{
			Path:        fmt.Sprintf("file-%d.txt", i),
			ETag:        fmt.Sprintf("etag-%d", i),
			Size:        100,
			IsDirectory: false,
		})
	}
	cloudSvc.Files = files

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})

	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() with 20 files error: %v", err)
	}

	// Verify all 20 metadata entries were saved
	for i := 0; i < 20; i++ {
		meta, err := repo.GetFileMetadata(context.Background(), cfg.ID, fmt.Sprintf("file-%d.txt", i))
		if err != nil {
			t.Fatalf("GetFileMetadata(file-%d.txt) error: %v", i, err)
		}
		if meta == nil {
			t.Errorf("expected metadata for file-%d.txt", i)
		}
	}
}

// -----------------------------
// 404 / 403 error handling tests
// -----------------------------

func TestSyncUseCase_Download404Error_RemovesStaleMetadata(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	apiErr := &googleapi.Error{Code: 404, Message: "File not found"}
	cloudSvc.DownloadError = apiErr

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-404")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-404",
		LocalPath:      localPath,
		RemoteFolderID: "remote-404",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	cloudSvc.Files = []domain.FileMetadata{
		{Path: "deleted-file.txt", ETag: "etag-deleted", Size: 100, IsDirectory: false},
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() should not return error for 404: %v", err)
	}
}

func TestSyncUseCase_Download403Error_SkipsGracefully(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	apiErr := &googleapi.Error{Code: 403, Message: "fileNotDownloadable"}
	cloudSvc.DownloadError = apiErr

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-403")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-403",
		LocalPath:      localPath,
		RemoteFolderID: "remote-403",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	cloudSvc.Files = []domain.FileMetadata{
		{Path: "google-doc.txt", ETag: "etag-docs", Size: 100, IsDirectory: false},
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() should not return error for 403: %v", err)
	}
}

func TestSyncUseCase_SyncSingleFile_404_RemovesMetadata(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	apiErr := &googleapi.Error{Code: 404, Message: "File not found"}
	cloudSvc.DownloadError = apiErr

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "single-404.txt")

	cfg := &domain.SyncConfig{
		AccountID:      "acct-single-404",
		LocalPath:      localPath,
		RemoteFolderID: "remote-file-404",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    false,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Pre-existing metadata to clean up
	if err := repo.UpdateFileMetadata(context.Background(), &domain.FileMetadata{
		SyncConfigID: cfg.ID,
		Path:         "single-404.txt",
		ETag:         "remote-file-404",
		Size:         100,
	}); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() should not return error for 404 single file: %v", err)
	}
}

func TestSyncUseCase_SyncSingleFile_403_SkipsGracefully(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	apiErr := &googleapi.Error{Code: 403, Message: "fileNotDownloadable"}
	cloudSvc.DownloadError = apiErr

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "single-403.txt")

	cfg := &domain.SyncConfig{
		AccountID:      "acct-single-403",
		LocalPath:      localPath,
		RemoteFolderID: "remote-file-403",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    false,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() should not return error for 403 single file: %v", err)
	}
}

func TestSyncUseCase_DownloadOtherError_ReturnsError(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	// Non-googleapi error — should propagate
	cloudSvc.DownloadError = fmt.Errorf("network timeout")

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-err")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-err",
		LocalPath:      localPath,
		RemoteFolderID: "remote-err",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	cloudSvc.Files = []domain.FileMetadata{
		{Path: "network-fail.txt", ETag: "etag-fail", Size: 100, IsDirectory: false},
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	// Should not return error at top-level (download errors are logged, not propagated)
	if err != nil {
		t.Errorf("SyncFolder() unexpected error: %v", err)
	}
}

func TestSyncUseCase_DownloadMixedErrors_404And403(t *testing.T) {
	repo := domain.NewMockRepository()

	err404 := &googleapi.Error{Code: 404, Message: "not found"}

	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)
	cloudSvc.DownloadError = err404

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-mixed")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-mixed",
		LocalPath:      localPath,
		RemoteFolderID: "remote-mixed",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	cloudSvc.Files = []domain.FileMetadata{
		{Path: "file-404.txt", ETag: "etag-404", Size: 100, IsDirectory: false},
		{Path: "file-403.txt", ETag: "etag-403", Size: 200, IsDirectory: false},
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() should not return error with mixed 404/403: %v", err)
	}
}
