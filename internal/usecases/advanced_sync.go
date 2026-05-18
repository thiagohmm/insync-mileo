package usecases

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

// SyncAllAdvanced syncs all folders with advanced features including task queue
func (s *syncUseCase) SyncAllAdvanced(ctx context.Context) error {
	configs, err := s.repo.ListSyncConfigs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sync configs: %w", err)
	}

	for _, config := range configs {
		// Enqueue sync tasks with priorities based on file count and size
		tasks, err := s.createSyncTasks(ctx, config)
		if err != nil {
			log.Printf("Error creating sync tasks for %s: %v", config.LocalPath, err)
			continue
		}

		for _, task := range tasks {
			if err := s.repo.SaveSyncTask(ctx, task); err != nil {
				log.Printf("Error saving task: %v", err)
			}
		}
	}

	// Process the task queue
	return s.ProcessTaskQueue(ctx)
}

// ProcessTaskQueue processes sync tasks based on priority
func (s *syncUseCase) ProcessTaskQueue(ctx context.Context) error {
	// Get tasks ordered by priority and creation time
	tasks, err := s.repo.GetNextSyncTask(ctx, 10) // Get next 10 tasks
	if err != nil {
		return fmt.Errorf("failed to get tasks: %w", err)
	}

	for _, task := range tasks {
		// Update task status to running
		task.Status = domain.TaskRunning
		task.UpdatedAt = time.Now()
		if err := s.repo.UpdateSyncTask(ctx, &task); err != nil {
			log.Printf("Error updating task status: %v", err)
			continue
		}

		// Process the task based on its type
		if err := s.executeTask(ctx, &task); err != nil {
			task.Status = domain.TaskFailed
			task.Error = err.Error()
		} else {
			task.Status = domain.TaskCompleted
		}

		task.UpdatedAt = time.Now()
		if err := s.repo.UpdateSyncTask(ctx, &task); err != nil {
			log.Printf("Error updating task status: %v", err)
		}
	}

	return nil
}

// executeTask executes a single sync task
func (s *syncUseCase) executeTask(ctx context.Context, task *domain.SyncTask) error {
	switch task.TaskType {
	case domain.DownloadTask:
		return s.executeDownloadTask(ctx, task)
	case domain.UploadTask:
		return s.executeUploadTask(ctx, task)
	case domain.DeleteLocalTask:
		return s.executeDeleteLocalTask(ctx, task)
	case domain.DeleteRemoteTask:
		return s.executeDeleteRemoteTask(ctx, task)
	case domain.VerifyChecksumTask:
		return s.executeVerifyChecksumTask(ctx, task)
	default:
		return fmt.Errorf("unknown task type: %d", task.TaskType)
	}
}

// executeDownloadTask executes a download task
func (s *syncUseCase) executeDownloadTask(ctx context.Context, task *domain.SyncTask) error {
	config, err := s.GetSyncConfig(ctx, task.SyncConfigID)
	if err != nil {
		return fmt.Errorf("failed to get sync config: %w", err)
	}

	cloudSvc, err := s.cloudForConfig(ctx, *config)
	if err != nil {
		return err
	}

	// Get remote file metadata
	remoteFiles, err := cloudSvc.ListFiles(ctx, config.RemoteFolderID)
	if err != nil {
		return fmt.Errorf("failed to list remote files: %w", err)
	}

	var remoteFile *domain.FileMetadata
	for _, f := range remoteFiles {
		if f.ETag == task.RemoteID {
			remoteFile = &f
			break
		}
	}

	if remoteFile == nil {
		return fmt.Errorf("remote file not found: %s", task.RemoteID)
	}

	localPath := task.FilePath
	if err := s.downloadFileWithProgress(ctx, *remoteFile, localPath, *config, cloudSvc); err != nil {
		return err
	}

	// Update file metadata
	if err := s.repo.UpdateFileMetadata(ctx, &domain.FileMetadata{
		SyncConfigID: config.ID,
		Path:         remoteFile.Path,
		ETag:         remoteFile.ETag,
		Size:         remoteFile.Size,
		LastModified: remoteFile.LastModified,
		IsDirectory:  false,
	}); err != nil {
		return fmt.Errorf("failed to update file metadata: %w", err)
	}

	return nil
}

// executeUploadTask executes an upload task
func (s *syncUseCase) executeUploadTask(ctx context.Context, task *domain.SyncTask) error {
	config, err := s.GetSyncConfig(ctx, task.SyncConfigID)
	if err != nil {
		return fmt.Errorf("failed to get sync config: %w", err)
	}

	cloudSvc, err := s.cloudForConfig(ctx, *config)
	if err != nil {
		return err
	}

	etag, err := cloudSvc.UploadFile(ctx, task.FilePath, config.RemoteFolderID)
	if err != nil {
		return fmt.Errorf("failed to upload file: %w", err)
	}

	// Get file info
	fileInfo, err := os.Stat(task.FilePath)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	// Update file metadata
	if err := s.repo.UpdateFileMetadata(ctx, &domain.FileMetadata{
		SyncConfigID: config.ID,
		Path:         filepath.Base(task.FilePath),
		ETag:         etag,
		Size:         fileInfo.Size(),
		LastModified: fileInfo.ModTime(),
		IsDirectory:  false,
	}); err != nil {
		return fmt.Errorf("failed to update file metadata: %w", err)
	}

	return nil
}

// executeDeleteLocalTask executes a local delete task
func (s *syncUseCase) executeDeleteLocalTask(ctx context.Context, task *domain.SyncTask) error {
	config, err := s.GetSyncConfig(ctx, task.SyncConfigID)
	if err != nil {
		return fmt.Errorf("failed to get sync config: %w", err)
	}

	// Delete local file
	if err := os.Remove(task.FilePath); err != nil {
		return fmt.Errorf("failed to delete local file: %w", err)
	}

	// Delete file metadata
	if err := s.repo.DeleteFileMetadata(ctx, config.ID, task.FilePath); err != nil {
		return fmt.Errorf("failed to delete file metadata: %w", err)
	}

	return nil
}

// executeDeleteRemoteTask executes a remote delete task
func (s *syncUseCase) executeDeleteRemoteTask(ctx context.Context, task *domain.SyncTask) error {
	config, err := s.GetSyncConfig(ctx, task.SyncConfigID)
	if err != nil {
		return fmt.Errorf("failed to get sync config: %w", err)
	}

	cloudSvc, err := s.cloudForConfig(ctx, *config)
	if err != nil {
		return err
	}

	// Delete remote file
	if err := cloudSvc.DeleteFile(ctx, task.RemoteID); err != nil {
		return fmt.Errorf("failed to delete remote file: %w", err)
	}

	// Delete file metadata
	if err := s.repo.DeleteFileMetadata(ctx, config.ID, task.FilePath); err != nil {
		return fmt.Errorf("failed to delete file metadata: %w", err)
	}

	return nil
}

// executeVerifyChecksumTask executes a checksum verification task
func (s *syncUseCase) executeVerifyChecksumTask(ctx context.Context, task *domain.SyncTask) error {
	cloudSvc, ok := s.cloudServices[domain.GoogleDrive]
	if !ok {
		return fmt.Errorf("cloud service not found")
	}

	// Get checksum from cloud provider
	remoteChecksum, err := cloudSvc.GetFileChecksum(ctx, task.RemoteID)
	if err != nil {
		return fmt.Errorf("failed to get remote checksum: %w", err)
	}

	// Compute local checksum
	localChecksum, err := computeFileChecksum(task.FilePath)
	if err != nil {
		return fmt.Errorf("failed to compute local checksum: %w", err)
	}

	// Compare checksums
	if localChecksum != remoteChecksum {
		return fmt.Errorf("checksum mismatch: local=%s, remote=%s", localChecksum, remoteChecksum)
	}

	return nil
}

// computeFileChecksum computes MD5 checksum of a file
func computeFileChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("failed to compute checksum: %w", err)
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// createSyncTasks creates sync tasks for a configuration
func (s *syncUseCase) createSyncTasks(ctx context.Context, config domain.SyncConfig) ([]*domain.SyncTask, error) {
	var tasks []*domain.SyncTask

	// List remote files
	cloudSvc, err := s.cloudForConfig(ctx, config)
	if err != nil {
		return nil, err
	}

	remoteFiles, err := cloudSvc.ListFiles(ctx, config.RemoteFolderID)
	if err != nil {
		return nil, fmt.Errorf("failed to list remote files: %w", err)
	}

	// Create download tasks for new/changed files
	for _, remote := range remoteFiles {
		if remote.IsDirectory {
			continue
		}

		metadata, err := s.repo.GetFileMetadata(ctx, config.ID, remote.Path)
		if err != nil || metadata == nil || metadata.ETag != remote.ETag {
			// Determine priority based on file size
			priority := domain.NormalPriority
			if remote.Size > 100*1024*1024 { // > 100MB
				priority = domain.HighPriority
			} else if remote.Size < 1024*1024 { // < 1MB
				priority = domain.LowPriority
			}

			tasks = append(tasks, &domain.SyncTask{
				SyncConfigID: config.ID,
				FilePath:     filepath.Join(config.LocalPath, remote.Path),
				RemoteID:     remote.ETag,
				FileSize:     remote.Size,
				TaskType:     domain.DownloadTask,
				Priority:     priority,
				CreatedAt:    time.Now(),
				UpdatedAt:    time.Now(),
			})
		}
	}

	return tasks, nil
}

// HandleWebhookNotification processes webhook notifications from cloud providers
func (s *syncUseCase) HandleWebhookNotification(notification *domain.PushNotification) error {
	// Find sync configurations that might be affected
	configs, err := s.repo.ListSyncConfigs(context.Background())
	if err != nil {
		return fmt.Errorf("failed to list sync configs: %w", err)
	}

	// For each configuration, check if the changed file/folder should be synced
	for _, config := range configs {
		// Determine if the notification affects this sync configuration
		// In a real implementation, this would check the notification.ResourceID
		// against the config.RemoteFolderID

		// For now, trigger a sync for the affected folder
		log.Printf("Webhook notification for %s: %s", config.LocalPath, notification.Changed)
		go func(c domain.SyncConfig) {
			if err := s.SyncFolder(context.Background(), c); err != nil {
				log.Printf("Error syncing after webhook: %v", err)
			}
		}(config)
	}

	return nil
}

// RegisterWebhook registers a webhook with the cloud provider
func (s *syncUseCase) RegisterWebhook(ctx context.Context, config domain.SyncConfig) error {
	// In a real implementation, this would call the cloud provider's API
	// to register a webhook. For now, we just save the configuration.

	// Check if webhook already exists
	existing, err := s.repo.GetWebhookConfig(ctx, config.ID)
	if err == nil && existing != nil {
		log.Printf("Webhook already registered for config %d", config.ID)
		return nil
	}

	// Generate a unique channel ID
	channelID := fmt.Sprintf("channel-%d-%d", config.ID, time.Now().Unix())

	// Save webhook configuration
	webhookConfig := &domain.WebhookConfig{
		SyncConfigID: config.ID,
		ChannelID:    channelID,
		ResourceID:   config.RemoteFolderID,
		Expiration:   time.Now().Add(7 * 24 * time.Hour), // 7 days
		EventType:    "push_notification",
	}

	if err := s.repo.SaveWebhookConfig(ctx, webhookConfig); err != nil {
		return fmt.Errorf("failed to save webhook config: %w", err)
	}

	log.Printf("Webhook registered for config %d with channel %s", config.ID, channelID)
	return nil
}

// ComputeAndStoreChecksum computes and stores the checksum of a file
func (s *syncUseCase) ComputeAndStoreChecksum(ctx context.Context, filePath string) (string, error) {
	// Compute checksum
	checksum, err := computeFileChecksum(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to compute checksum: %w", err)
	}

	// Store in deduplication table
	key := &domain.FileDeduplicationKey{
		MD5Checksum: checksum,
	}

	if err := s.repo.SaveDedupKey(ctx, key); err != nil {
		return "", fmt.Errorf("failed to store dedup key: %w", err)
	}

	return checksum, nil
}

// DeduplicateFolder finds and reports duplicate files in a folder
func (s *syncUseCase) DeduplicateFolder(ctx context.Context, folderPath string) ([]string, error) {
	var duplicates []string

	// Walk through the folder
	err := filepath.Walk(folderPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Compute checksum
		checksum, err := computeFileChecksum(path)
		if err != nil {
			log.Printf("Failed to compute checksum for %s: %v", path, err)
			return nil
		}

		// Check for duplicates
		key := &domain.FileDeduplicationKey{
			MD5Checksum: checksum,
			Size:        info.Size(),
		}

		existing, err := s.repo.GetDedupKey(ctx, checksum, info.Size())
		if err != nil {
			return err
		}

		if existing != nil {
			files, err := s.repo.ListFilesByDedupKey(ctx, existing)
			if err != nil {
				return err
			}
			if len(files) > 0 {
				duplicates = append(duplicates, fmt.Sprintf("%s is duplicate of %s", path, files[0].Path))
			} else {
				duplicates = append(duplicates, fmt.Sprintf("%s has duplicate checksum %s", path, checksum))
			}
		} else {
			// Store the key for future duplicate detection
			if err := s.repo.SaveDedupKey(ctx, key); err != nil {
				log.Printf("Failed to store dedup key for %s: %v", path, err)
			}
		}

		return nil
	})

	return duplicates, err
}

// SyncUseCase interface implementation for new methods
var _ domain.SyncUseCase = (*syncUseCase)(nil)

// GetSyncConfig is a helper method to get a sync config by ID
func (s *syncUseCase) GetSyncConfig(ctx context.Context, id int64) (*domain.SyncConfig, error) {
	configs, err := s.repo.ListSyncConfigs(ctx)
	if err != nil {
		return nil, err
	}

	for _, config := range configs {
		if config.ID == id {
			return &config, nil
		}
	}

	return nil, fmt.Errorf("sync config not found: %d", id)
}
