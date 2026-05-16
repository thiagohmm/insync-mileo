package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

func TestNewRemotePoller(t *testing.T) {
	repo := domain.NewMockRepository()
	suc := domain.NewMockSyncUseCase()
	interval := 5 * time.Second

	poller := NewRemotePoller(suc, repo, interval)
	if poller == nil {
		t.Fatal("expected non-nil poller")
	}
}

func TestRemotePoller_Poll_NoConfigs(t *testing.T) {
	repo := domain.NewMockRepository()
	suc := domain.NewMockSyncUseCase()
	poller := NewRemotePoller(suc, repo, time.Second)

	ctx := context.Background()
	poller.poll(ctx)

	if len(suc.SyncedConfigs) != 0 {
		t.Errorf("expected no synced configs, got %d", len(suc.SyncedConfigs))
	}
}

func TestRemotePoller_Poll_WithConfigs(t *testing.T) {
	repo := domain.NewMockRepository()
	suc := domain.NewMockSyncUseCase()
	poller := NewRemotePoller(suc, repo, time.Second)

	ctx := context.Background()

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      "/tmp/poll-test",
		RemoteFolderID: "remote-poll",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	poller.poll(ctx)

	if len(suc.SyncedConfigs) != 1 {
		t.Errorf("expected 1 synced config, got %d", len(suc.SyncedConfigs))
	}
}

func TestRemotePoller_Poll_SyncError(t *testing.T) {
	repo := domain.NewMockRepository()
	suc := domain.NewMockSyncUseCase()
	suc.SyncError = fmt.Errorf("sync failed")
	poller := NewRemotePoller(suc, repo, time.Second)

	ctx := context.Background()

	cfg := &domain.SyncConfig{
		AccountID:      "acct-1",
		LocalPath:      "/tmp/poll-err",
		RemoteFolderID: "remote-poll-err",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Should not panic even with sync error
	poller.poll(ctx)
}

func TestRemotePoller_Poll_ListConfigsError(t *testing.T) {
	repo := domain.NewMockRepository()
	repo.ErrorOn["ListSyncConfigs"] = fmt.Errorf("db error")
	suc := domain.NewMockSyncUseCase()
	poller := NewRemotePoller(suc, repo, time.Second)

	// Should not panic
	poller.poll(context.Background())
}

func TestRemotePoller_Start_Stop(t *testing.T) {
	repo := domain.NewMockRepository()
	suc := domain.NewMockSyncUseCase()
	poller := NewRemotePoller(suc, repo, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Start in goroutine
	go poller.Start(ctx)

	// Wait for context to expire (poller should stop)
	<-ctx.Done()
}

func TestRemotePoller_Poll_MultipleConfigs(t *testing.T) {
	repo := domain.NewMockRepository()
	suc := domain.NewMockSyncUseCase()
	poller := NewRemotePoller(suc, repo, time.Second)

	ctx := context.Background()

	for i := 0; i < 3; i++ {
		cfg := &domain.SyncConfig{
			AccountID:      "acct-multi",
			LocalPath:      fmt.Sprintf("/tmp/poll-multi-%d", i),
			RemoteFolderID: fmt.Sprintf("remote-multi-%d", i),
			Mode:           domain.BaseSync,
			Provider:       domain.GoogleDrive,
			IsDirectory:    true,
		}
		if err := repo.SaveSyncConfig(ctx, cfg); err != nil {
			t.Fatalf("SaveSyncConfig() error: %v", err)
		}
	}

	poller.poll(ctx)

	if len(suc.SyncedConfigs) != 3 {
		t.Errorf("expected 3 synced configs, got %d", len(suc.SyncedConfigs))
	}
}
