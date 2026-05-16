package usecases

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/thiagohmm/insync-clone/internal/adapters/cloud"
	"github.com/thiagohmm/insync-clone/internal/domain"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
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

func (s *syncUseCase) Statuses() <-chan domain.SyncStatus {
	return s.statusChan
}

func (s *syncUseCase) SyncFolder(ctx context.Context, config domain.SyncConfig) error {
	cloudSvc, err := s.cloudForConfig(ctx, config)
	if err != nil {
		return err
	}

	if !config.IsDirectory {
		return s.syncSingleFile(ctx, config, cloudSvc)
	}

	remoteFiles, err := cloudSvc.ListFiles(ctx, config.RemoteFolderID)
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
	var downloadWG sync.WaitGroup
	downloadSem := make(chan struct{}, 4)
	for _, remote := range remoteFiles {
		if remote.IsDirectory {
			delete(localFiles, remote.Path)
			continue
		}
		localPath, errPath := safeJoinLocal(config.LocalPath, remote.Path)
		if errPath != nil {
			return errPath
		}
		metadata, errMeta := s.repo.GetFileMetadata(ctx, config.ID, remote.Path)

		if errMeta != nil || metadata == nil || metadata.ETag != remote.ETag {
			remote := remote
			localPath := localPath
			downloadWG.Add(1)
			go func() {
				defer downloadWG.Done()
				downloadSem <- struct{}{}
				defer func() { <-downloadSem }()

				if err := s.downloadFileWithProgress(ctx, remote, localPath, config, cloudSvc); err != nil {
					s.sendStatus(remote.Path, "Error", 0, remote.Size)
					return
				}
				rec := remote
				rec.SyncConfigID = config.ID
				if err := s.repo.UpdateFileMetadata(ctx, &rec); err != nil {
					log.Printf("UpdateFileMetadata %s: %v", remote.Path, err)
				}
				s.sendStatus(remote.Path, "Synced", 100, remote.Size)
			}()
		}
		delete(localFiles, remote.Path)
	}
	downloadWG.Wait()

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
		s.sendStatus(relPath, "Removed local (deleted in cloud)", 100, md.Size)
	}

	// 3) Local → nuvem: ficheiros novos (sem metadados)
	var uploadWG sync.WaitGroup
	uploadSem := make(chan struct{}, 4)
	for relPath, info := range localFiles {
		if info.IsDir() {
			continue
		}

		localFullPath := filepath.Join(config.LocalPath, relPath)
		metadata, _ := s.repo.GetFileMetadata(ctx, config.ID, relPath)

		if metadata == nil {
			relPath := relPath
			info := info
			localFullPath := localFullPath
			uploadWG.Add(1)
			go func() {
				defer uploadWG.Done()
				uploadSem <- struct{}{}
				defer func() { <-uploadSem }()

				s.sendStatus(relPath, "Uploading", 0, info.Size())
				etag, err := cloudSvc.UploadFile(ctx, localFullPath, config.RemoteFolderID)
				if err != nil {
					s.sendStatus(relPath, "Error", 0, info.Size())
					return
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
				s.sendStatus(relPath, "Synced", 100, info.Size())
			}()
		}
	}
	uploadWG.Wait()

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
			if err := cloudSvc.DeleteFile(ctx, md.ETag); err != nil {
				s.sendStatus(md.Path, "Error deleting in cloud", 0, md.Size)
				continue
			}
			s.sendStatus(md.Path, "Deleted in cloud (full-sync)", 100, md.Size)
		default:
			s.sendStatus(md.Path, "Local delete — cloud kept (base-sync)", 100, md.Size)
		}

		if err := s.repo.DeleteFileMetadata(ctx, config.ID, md.Path); err != nil {
			log.Printf("DeleteFileMetadata missing local %s: %v", md.Path, err)
		}
	}

	return nil
}

func (s *syncUseCase) syncSingleFile(ctx context.Context, config domain.SyncConfig, cloudSvc domain.CloudService) error {
	mdList, err := s.repo.ListFileMetadata(ctx, config.ID)
	if err != nil {
		return fmt.Errorf("list file metadata: %w", err)
	}
	var md *domain.FileMetadata
	if len(mdList) > 0 {
		md = &mdList[0]
	}

	if _, statErr := os.Stat(config.LocalPath); statErr == nil {
		return nil
	} else if !os.IsNotExist(statErr) {
		return statErr
	}

	size := int64(0)
	remoteID := config.RemoteFolderID
	pathLabel := filepath.Base(config.LocalPath)
	if md != nil {
		size = md.Size
		remoteID = md.ETag
		pathLabel = md.Path
	}

	switch config.Mode {
	case domain.FullSync:
		if err := cloudSvc.DeleteFile(ctx, remoteID); err != nil {
			s.sendStatus(pathLabel, "Error deleting in cloud", 0, size)
			return err
		}
		s.sendStatus(pathLabel, "Deleted in cloud (full-sync)", 100, size)
	default:
		s.sendStatus(pathLabel, "Local delete — cloud kept (base-sync)", 100, size)
	}

	if md != nil {
		if err := s.repo.DeleteFileMetadata(ctx, config.ID, md.Path); err != nil {
			log.Printf("DeleteFileMetadata missing local %s: %v", md.Path, err)
		}
	}
	return nil
}

func (s *syncUseCase) cloudForConfig(ctx context.Context, config domain.SyncConfig) (domain.CloudService, error) {
	if svc, ok := s.cloudServices[config.Provider]; ok {
		return svc, nil
	}
	acc, err := s.repo.GetAccount(ctx, config.AccountID)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}
	if acc == nil {
		return nil, fmt.Errorf("account %s not found", config.AccountID)
	}

	cfg := &oauth2.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		Endpoint:     google.Endpoint,
		RedirectURL:  "http://localhost:8080",
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "1092767661178-2a973gcsj0cip2oknvdkpsl31vugqrp4.apps.googleusercontent.com"
	}
	tok := &oauth2.Token{
		AccessToken:  acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		Expiry:       acc.Expiry.Time,
	}
	driveSvc, err := drive.NewService(ctx, option.WithHTTPClient(cfg.Client(ctx, tok)))
	if err != nil {
		return nil, err
	}
	return cloud.NewGoogleDriveService(driveSvc), nil
}

// downloadFileWithProgress downloads a file from the cloud while sending progress updates.
// Progress is estimated based on bytes downloaded vs total file size.
func (s *syncUseCase) downloadFileWithProgress(ctx context.Context, remote domain.FileMetadata, localPath string, config domain.SyncConfig, cloudSvc domain.CloudService) error {
	s.sendStatus(remote.Path, "Downloading", 0, remote.Size)

	err := cloudSvc.DownloadFileWithProgress(ctx, remote.ETag, localPath, func(d, total int64) {
		if total > 0 {
			progress := int32(float64(d) / float64(total) * 100)
			s.sendStatus(remote.Path, "Downloading", progress, total)
		}
	})

	return err
}

func (s *syncUseCase) sendStatus(path, status string, progress int32, size int64) {
	msg := domain.SyncStatus{
		FilePath:           path,
		Status:             status,
		ProgressPercentage: progress,
		TotalSize:          size,
		ProcessedSize:      size * int64(progress) / 100,
	}
	select {
	case s.statusChan <- msg:
	default:
	}
}

func safeJoinLocal(base string, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("nome remoto inválido: %q", name)
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) || filepath.Clean(name) != name {
		return "", fmt.Errorf("nome remoto inseguro: %q", name)
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	joined := filepath.Clean(filepath.Join(baseAbs, name))
	if joined != baseAbs && !strings.HasPrefix(joined, baseAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("caminho remoto escapa da raiz local: %q", name)
	}
	return joined, nil
}
