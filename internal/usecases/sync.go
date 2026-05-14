package usecases

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

type syncUseCase struct {
	repo          domain.Repository
	cloudServices map[domain.Provider]domain.CloudService
	statusChan    chan domain.SyncStatus
}

func NewSyncUseCase(repo domain.Repository, clouds []domain.CloudService) domain.SyncUseCase {
	cloudMap := make(map[domain.Provider]domain.CloudService)
	for _, c := range clouds {
		cloudMap[c.GetProvider()] = c
	}
	return &syncUseCase{
		repo:          repo,
		cloudServices: cloudMap,
		statusChan:    make(chan domain.SyncStatus, 100),
	}
}

func (s *syncUseCase) SyncAll(ctx context.Context) error {
	configs, err := s.repo.ListSyncConfigs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sync configs: %w", err)
	}

	for _, config := range configs {
		if err := s.SyncFolder(ctx, config); err != nil {
			log.Printf("Error syncing folder %s: %v", config.LocalPath, err)
		}
	}

	return nil
}

func (s *syncUseCase) SyncFolder(ctx context.Context, config domain.SyncConfig) error {
	cloud, ok := s.cloudServices[config.Provider]
	if !ok {
		return fmt.Errorf("cloud service not found for provider %s", config.Provider)
	}

	remoteFiles, err := cloud.ListFiles(ctx, config.RemoteFolderID)
	if err != nil {
		return fmt.Errorf("failed to list remote files: %w", err)
	}

	localFiles := make(map[string]os.FileInfo)
	err = filepath.Walk(config.LocalPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == config.LocalPath {
			return nil
		}
		relPath, _ := filepath.Rel(config.LocalPath, path)
		localFiles[relPath] = info
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk local path: %w", err)
	}

	// 1) Nuvem → local: download novos/alterados
	for _, remote := range remoteFiles {
		if remote.IsDirectory {
			delete(localFiles, remote.Path)
			continue
		}
		localPath := filepath.Join(config.LocalPath, remote.Path)
		metadata, errMeta := s.repo.GetFileMetadata(ctx, config.ID, remote.Path)

		if errMeta != nil || metadata == nil || metadata.ETag != remote.ETag {
			s.sendStatus(remote.Path, "Downloading", 0, remote.Size, config.Provider)
			if err := cloud.DownloadFile(ctx, remote.ETag, localPath); err != nil {
				s.sendStatus(remote.Path, "Error", 0, remote.Size, config.Provider)
				delete(localFiles, remote.Path)
				continue
			}
			rec := remote
			rec.SyncConfigID = config.ID
			if err := s.repo.UpdateFileMetadata(ctx, &rec); err != nil {
				log.Printf("UpdateFileMetadata %s: %v", remote.Path, err)
			}
			s.sendStatus(remote.Path, "Synced", 100, remote.Size, config.Provider)
		}
		delete(localFiles, remote.Path)
	}

	// 2) Apagado na nuvem: ficheiro ainda em disco e com metadados → remove cópia local (base e full)
	for relPath, info := range localFiles {
		if info.IsDir() {
			continue
		}
		md, _ := s.repo.GetFileMetadata(ctx, config.ID, relPath)
		if md == nil {
			continue
		}
		localFullPath := filepath.Join(config.LocalPath, relPath)
		_ = os.Remove(localFullPath)
		if err := s.repo.DeleteFileMetadata(ctx, config.ID, relPath); err != nil {
			log.Printf("DeleteFileMetadata %s: %v", relPath, err)
		}
		delete(localFiles, relPath)
		s.sendStatus(relPath, "Removed local (deleted in cloud)", 100, md.Size, config.Provider)
	}

	// 3) Local → nuvem: ficheiros novos (sem metadados)
	for relPath, info := range localFiles {
		if info.IsDir() {
			continue
		}

		localFullPath := filepath.Join(config.LocalPath, relPath)
		metadata, _ := s.repo.GetFileMetadata(ctx, config.ID, relPath)

		if metadata == nil {
			s.sendStatus(relPath, "Uploading", 0, info.Size(), config.Provider)
			etag, err := cloud.UploadFile(ctx, localFullPath, config.RemoteFolderID)
			if err != nil {
				s.sendStatus(relPath, "Error", 0, info.Size(), config.Provider)
				continue
			}

			if err := s.repo.UpdateFileMetadata(ctx, &domain.FileMetadata{
				SyncConfigID: config.ID,
				Path:         relPath,
				ETag:         etag,
				Size:         info.Size(),
				LastModified: info.ModTime(),
				IsDirectory:  false,
			}); err != nil {
				log.Printf("UpdateFileMetadata upload %s: %v", relPath, err)
			}
			s.sendStatus(relPath, "Synced", 100, info.Size(), config.Provider)
		}
	}

	// 4) Apagado só local: ficheiro sumiu do disco mas ainda há metadados
	metaList, err := s.repo.ListFileMetadata(ctx, config.ID)
	if err != nil {
		return fmt.Errorf("list file metadata: %w", err)
	}
	for _, md := range metaList {
		if md.IsDirectory {
			continue
		}
		localFull := filepath.Join(config.LocalPath, md.Path)
		if _, statErr := os.Stat(localFull); statErr == nil || !os.IsNotExist(statErr) {
			continue
		}

		switch config.Mode {
		case domain.FullSync:
			if err := cloud.DeleteFile(ctx, md.ETag); err != nil {
				s.sendStatus(md.Path, "Error deleting in cloud", 0, md.Size, config.Provider)
				continue
			}
			s.sendStatus(md.Path, "Deleted in cloud (full-sync)", 100, md.Size, config.Provider)
		default:
			s.sendStatus(md.Path, "Local delete — cloud kept (base-sync)", 100, md.Size, config.Provider)
		}

		if err := s.repo.DeleteFileMetadata(ctx, config.ID, md.Path); err != nil {
			log.Printf("DeleteFileMetadata missing local %s: %v", md.Path, err)
		}
	}

	return nil
}

func (s *syncUseCase) sendStatus(path, status string, progress int32, size int64, provider domain.Provider) {
	s.statusChan <- domain.SyncStatus{
		FilePath:           path,
		Status:             status,
		ProgressPercentage: progress,
		TotalSize:          size,
		ProcessedSize:      size * int64(progress) / 100,
		Provider:           provider,
	}
}
