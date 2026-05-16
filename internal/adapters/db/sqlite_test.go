package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

func newTestDB(t *testing.T) (*SQLiteRepository, func()) {
	t.Helper()

	tmpFile := fmt.Sprintf("test_db_%d.db", time.Now().UnixNano())
	repo, err := NewSQLiteRepository(tmpFile)
	if err != nil {
		t.Fatalf("failed to create test repo: %v", err)
	}

	cleanup := func() {
		repo.Close()
		os.Remove(tmpFile)
	}

	return repo, cleanup
}

func TestNewSQLiteRepository(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	if repo == nil {
		t.Fatal("expected non-nil repository")
	}
}

func TestNewSQLiteRepositoryRejectsSymlinkPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.db")
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	repo, err := NewSQLiteRepository(link)
	if err == nil {
		_ = repo.Close()
		t.Fatal("expected symlink database path to be rejected")
	}
}

func TestSQLiteRepository_SaveAndGetAccount(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	acc := &domain.Account{
		ID:           "test-account-1",
		Provider:     domain.GoogleDrive,
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		Expiry:       domain.NullableTime{Time: time.Now().Add(time.Hour), Valid: true},
	}

	if err := repo.SaveAccount(ctx, acc); err != nil {
		t.Fatalf("SaveAccount() error: %v", err)
	}

	got, err := repo.GetAccount(ctx, "test-account-1")
	if err != nil {
		t.Fatalf("GetAccount() error: %v", err)
	}
	if got == nil {
		t.Fatal("GetAccount() returned nil")
	}
	if got.ID != acc.ID {
		t.Errorf("ID = %s, want %s", got.ID, acc.ID)
	}
	if got.Provider != acc.Provider {
		t.Errorf("Provider = %s, want %s", got.Provider, acc.Provider)
	}
	if got.AccessToken != acc.AccessToken {
		t.Errorf("AccessToken = %s, want %s", got.AccessToken, acc.AccessToken)
	}
}

func TestSQLiteRepository_GetAccount_NotFound(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	got, err := repo.GetAccount(context.Background(), "non-existent")
	if err != nil {
		t.Fatalf("GetAccount() error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for non-existent account, got %v", got)
	}
}

func TestSQLiteRepository_GetLatestAccountByProvider(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	acc1 := &domain.Account{
		ID:           "acct-1",
		Provider:     domain.GoogleDrive,
		AccessToken:  "token1",
		RefreshToken: "refresh1",
	}
	acc2 := &domain.Account{
		ID:           "acct-2",
		Provider:     domain.GoogleDrive,
		AccessToken:  "token2",
		RefreshToken: "refresh2",
	}

	if err := repo.SaveAccount(ctx, acc1); err != nil {
		t.Fatalf("SaveAccount() error: %v", err)
	}
	if err := repo.SaveAccount(ctx, acc2); err != nil {
		t.Fatalf("SaveAccount() error: %v", err)
	}

	got, err := repo.GetLatestAccountByProvider(ctx, domain.GoogleDrive)
	if err != nil {
		t.Fatalf("GetLatestAccountByProvider() error: %v", err)
	}
	if got == nil {
		t.Fatal("expected account, got nil")
	}
	// The second account should be the latest
	if got.ID != "acct-2" {
		t.Errorf("ID = %s, want acct-2", got.ID)
	}
}

func TestSQLiteRepository_GetLatestAccountByProvider_NotFound(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	got, err := repo.GetLatestAccountByProvider(context.Background(), domain.GoogleDrive)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestSQLiteRepository_SaveAndListSyncConfigs(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	cfg1 := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      "/tmp/sync1",
		RemoteFolderID: "remote-1",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	cfg2 := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      "/tmp/sync2",
		RemoteFolderID: "remote-2",
		Mode:           domain.FullSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    false,
	}

	if err := repo.SaveSyncConfig(ctx, cfg1); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}
	if err := repo.SaveSyncConfig(ctx, cfg2); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	configs, err := repo.ListSyncConfigs(ctx)
	if err != nil {
		t.Fatalf("ListSyncConfigs() error: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}

	// Verify IDs were assigned
	if cfg1.ID == 0 || cfg2.ID == 0 {
		t.Error("expected non-zero IDs after save")
	}
}

func TestSQLiteRepository_SaveSyncConfig_UpdateExisting(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      "/tmp/sync-update",
		RemoteFolderID: "remote-1",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}

	if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
		t.Fatalf("initial save error: %v", err)
	}
	originalID := cfg.ID

	// Update the config
	cfg.RemoteFolderID = "remote-updated"
	cfg.Mode = domain.FullSync

	if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
		t.Fatalf("update error: %v", err)
	}

	// Verify it updated, not created a new entry
	configs, err := repo.ListSyncConfigs(ctx)
	if err != nil {
		t.Fatalf("ListSyncConfigs() error: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config after update, got %d", len(configs))
	}
	if configs[0].RemoteFolderID != "remote-updated" {
		t.Errorf("RemoteFolderID = %s, want remote-updated", configs[0].RemoteFolderID)
	}
	if cfg.ID != originalID {
		t.Errorf("ID changed after update: got %d, want %d", cfg.ID, originalID)
	}
}

func TestSQLiteRepository_GetSyncConfigByPath(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      "/tmp/sync-by-path",
		RemoteFolderID: "remote-1",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}

	if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	got, err := repo.GetSyncConfigByPath(ctx, "/tmp/sync-by-path")
	if err != nil {
		t.Fatalf("GetSyncConfigByPath() error: %v", err)
	}
	if got == nil {
		t.Fatal("expected config, got nil")
	}
	if got.RemoteFolderID != "remote-1" {
		t.Errorf("RemoteFolderID = %s, want remote-1", got.RemoteFolderID)
	}
}

func TestSQLiteRepository_GetSyncConfigByPath_NotFound(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	got, err := repo.GetSyncConfigByPath(context.Background(), "/nonexistent")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestSQLiteRepository_DeleteSyncConfigByRemoteID(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	cfg := &domain.SyncConfig{
		AccountID:      "acct-to-delete",
		LocalPath:      "/tmp/sync-delete",
		RemoteFolderID: "remote-delete",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}

	if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Add file metadata
	if err := repo.UpdateFileMetadata(ctx, &domain.FileMetadata{
		SyncConfigID: cfg.ID,
		Path:         "test.txt",
		ETag:         "etag-123",
		Size:         100,
	}); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	if err := repo.DeleteSyncConfigByRemoteID(ctx, "acct-to-delete", "remote-delete"); err != nil {
		t.Fatalf("DeleteSyncConfigByRemoteID() error: %v", err)
	}

	// Verify deletion
	configs, err := repo.ListSyncConfigs(ctx)
	if err != nil {
		t.Fatalf("ListSyncConfigs() error: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("expected 0 configs after delete, got %d", len(configs))
	}
}

func TestSQLiteRepository_UpdateAndGetFileMetadata(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	metadata := &domain.FileMetadata{
		SyncConfigID: 1,
		Path:         "test.txt",
		ETag:         "etag-abc",
		Size:         1024,
		LastModified: now,
		IsDirectory:  false,
	}

	if err := repo.UpdateFileMetadata(ctx, metadata); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	got, err := repo.GetFileMetadata(ctx, 1, "test.txt")
	if err != nil {
		t.Fatalf("GetFileMetadata() error: %v", err)
	}
	if got == nil {
		t.Fatal("expected metadata, got nil")
	}
	if got.ETag != "etag-abc" {
		t.Errorf("ETag = %s, want etag-abc", got.ETag)
	}
	if got.Size != 1024 {
		t.Errorf("Size = %d, want 1024", got.Size)
	}
}

func TestSQLiteRepository_GetFileMetadata_NotFound(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	got, err := repo.GetFileMetadata(context.Background(), 999, "missing.txt")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestSQLiteRepository_DeleteFileMetadata(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	if err := repo.UpdateFileMetadata(ctx, &domain.FileMetadata{
		SyncConfigID: 1,
		Path:         "delete-me.txt",
		ETag:         "etag-456",
		Size:         500,
	}); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	if err := repo.DeleteFileMetadata(ctx, 1, "delete-me.txt"); err != nil {
		t.Fatalf("DeleteFileMetadata() error: %v", err)
	}

	got, err := repo.GetFileMetadata(ctx, 1, "delete-me.txt")
	if err != nil {
		t.Fatalf("GetFileMetadata() error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after delete, got %v", got)
	}
}

func TestSQLiteRepository_ListFileMetadata(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := repo.UpdateFileMetadata(ctx, &domain.FileMetadata{
			SyncConfigID: 1,
			Path:         fmt.Sprintf("file%d.txt", i),
			ETag:         fmt.Sprintf("etag-%d", i),
			Size:         int64(100 * (i + 1)),
		}); err != nil {
			t.Fatalf("UpdateFileMetadata() error: %v", err)
		}
	}

	metas, err := repo.ListFileMetadata(ctx, 1)
	if err != nil {
		t.Fatalf("ListFileMetadata() error: %v", err)
	}
	if len(metas) != 3 {
		t.Errorf("expected 3 metadata entries, got %d", len(metas))
	}
}

func TestSQLiteRepository_ListFileMetadata_Empty(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	metas, err := repo.ListFileMetadata(context.Background(), 999)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("expected 0 entries, got %d", len(metas))
	}
}

func TestSQLiteRepository_SaveAccount_ReplacesExisting(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	acc := &domain.Account{
		ID:           "replace-me",
		Provider:     domain.GoogleDrive,
		AccessToken:  "old-token",
		RefreshToken: "old-refresh",
	}

	if err := repo.SaveAccount(ctx, acc); err != nil {
		t.Fatalf("initial save error: %v", err)
	}

	// Update the same account
	acc.AccessToken = "new-token"
	acc.RefreshToken = "new-refresh"

	if err := repo.SaveAccount(ctx, acc); err != nil {
		t.Fatalf("replace error: %v", err)
	}

	got, err := repo.GetAccount(ctx, "replace-me")
	if err != nil {
		t.Fatalf("GetAccount() error: %v", err)
	}
	if got.AccessToken != "new-token" {
		t.Errorf("AccessToken = %s, want new-token", got.AccessToken)
	}
	if got.RefreshToken != "new-refresh" {
		t.Errorf("RefreshToken = %s, want new-refresh", got.RefreshToken)
	}
}

func TestSQLiteRepository_ConcurrentAccess(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	var wg sync.WaitGroup
	numberOfGoroutines := 10

	// Concurrent saves
	wg.Add(numberOfGoroutines)
	for i := 0; i < numberOfGoroutines; i++ {
		go func(i int) {
			defer wg.Done()
			acc := &domain.Account{
				ID:           fmt.Sprintf("concurrent-%d", i),
				Provider:     domain.GoogleDrive,
				AccessToken:  fmt.Sprintf("token-%d", i),
				RefreshToken: fmt.Sprintf("refresh-%d", i),
			}
			if err := repo.SaveAccount(ctx, acc); err != nil {
				t.Errorf("concurrent SaveAccount(%d) error: %v", i, err)
			}
		}(i)
	}

	// Concurrent sync config saves
	wg.Add(numberOfGoroutines)
	for i := 0; i < numberOfGoroutines; i++ {
		go func(i int) {
			defer wg.Done()
			cfg := &domain.SyncConfig{
				AccountID:      fmt.Sprintf("concurrent-%d", i),
				LocalPath:      fmt.Sprintf("/tmp/concurrent/%d", i),
				RemoteFolderID: fmt.Sprintf("remote-%d", i),
				Mode:           domain.BaseSync,
				Provider:       domain.GoogleDrive,
				IsDirectory:    true,
			}
			if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
				t.Errorf("concurrent SaveSyncConfig(%d) error: %v", i, err)
			}
		}(i)
	}

	wg.Wait()

	// Verify all accounts were saved
	configs, err := repo.ListSyncConfigs(ctx)
	if err != nil {
		t.Fatalf("ListSyncConfigs() error: %v", err)
	}
	if len(configs) != numberOfGoroutines {
		t.Errorf("expected %d configs, got %d", numberOfGoroutines, len(configs))
	}
}

func TestSQLiteRepository_FullWorkflow(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Save account
	acc := &domain.Account{
		ID:           "workflow-account",
		Provider:     domain.GoogleDrive,
		AccessToken:  "wf-token",
		RefreshToken: "wf-refresh",
		Expiry:       domain.NullableTime{Time: time.Now().Add(time.Hour), Valid: true},
	}
	if err := repo.SaveAccount(ctx, acc); err != nil {
		t.Fatalf("SaveAccount() error: %v", err)
	}

	// 2. Save sync config
	cfg := &domain.SyncConfig{
		AccountID:      "workflow-account",
		LocalPath:      "/tmp/workflow-sync",
		RemoteFolderID: "remote-workflow",
		Mode:           domain.FullSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// 3. Save file metadata
	if err := repo.UpdateFileMetadata(ctx, &domain.FileMetadata{
		SyncConfigID: cfg.ID,
		Path:         "workflow.txt",
		ETag:         "wf-etag",
		Size:         2048,
		LastModified: time.Now(),
	}); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	// 4. Verify all data exists
	gotAcc, err := repo.GetAccount(ctx, "workflow-account")
	if err != nil || gotAcc == nil {
		t.Fatal("account not found")
	}

	gotCfg, err := repo.GetSyncConfigByPath(ctx, "/tmp/workflow-sync")
	if err != nil || gotCfg == nil {
		t.Fatal("sync config not found")
	}

	gotMeta, err := repo.GetFileMetadata(ctx, cfg.ID, "workflow.txt")
	if err != nil || gotMeta == nil {
		t.Fatal("file metadata not found")
	}

	// 5. Clean up
	if err := repo.DeleteSyncConfigByRemoteID(ctx, "workflow-account", "remote-workflow"); err != nil {
		t.Fatalf("DeleteSyncConfigByRemoteID() error: %v", err)
	}

	configs, err := repo.ListSyncConfigs(ctx)
	if err != nil || len(configs) != 0 {
		t.Error("sync config not cleaned up properly")
	}
}

func TestSQLiteRepository_EmptyListSyncConfigs(t *testing.T) {
	repo, cleanup := newTestDB(t)
	defer cleanup()

	configs, err := repo.ListSyncConfigs(context.Background())
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("expected 0 configs, got %d", len(configs))
	}
}
