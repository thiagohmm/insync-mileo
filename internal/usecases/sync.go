package usecases

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/thiagohmm/insync-clone/internal/adapters/cloud"
	"github.com/thiagohmm/insync-clone/internal/domain"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
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
		listErr := strings.ToLower(err.Error())
		canFallbackToFile := strings.Contains(listErr, "notfound") || strings.Contains(listErr, "file not found")
		if canFallbackToFile {
			info, statErr := os.Stat(config.LocalPath)
			canFallbackToFile = os.IsNotExist(statErr) || (statErr == nil && !info.IsDir())
		}
		if canFallbackToFile {
			if fallbackErr := s.syncSingleFile(ctx, config, cloudSvc); fallbackErr == nil {
				return nil
			}
		}
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

	// 1) Nuvem → local: download novos/alterados (com conflito e checksum)
	downloadGrp, downloadCtx := errgroup.WithContext(ctx)
	downloadGrp.SetLimit(4)
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
			// Conflict detection: both remote changed (ETag differs) AND local was
			// modified since last sync (mod time > metadata.LastModified).
			if metadata != nil && metadata.ETag != remote.ETag {
				if localInfo, exists := localFiles[remote.Path]; exists {
					if localInfo.ModTime().After(metadata.LastModified) {
						s.sendStatus(remote.Path, "Conflict detected", 0, remote.Size)
						conflictPath := localPath + ".conflict"
						if err := s.downloadFileWithProgress(downloadCtx, remote, conflictPath, config, cloudSvc); err != nil {
							s.sendStatus(remote.Path, "Error creating conflict copy", 0, remote.Size)
						} else {
							s.sendStatus(remote.Path, "Conflict saved as .conflict", 100, remote.Size)
							log.Printf("conflict: %s saved as %s", remote.Path, conflictPath)
						}
						delete(localFiles, remote.Path)
						continue
					}
				}
			}

			remote := remote
			localPath := localPath
			downloadGrp.Go(func() error {
				if err := s.downloadFileWithProgress(downloadCtx, remote, localPath, config, cloudSvc); err != nil {
					if gErr, ok := err.(*googleapi.Error); ok && gErr.Code == 404 {
						log.Printf("skipping %s: file not found in cloud (deleted)", remote.Path)
						if errDel := s.repo.DeleteFileMetadata(downloadCtx, config.ID, remote.Path); errDel != nil {
							log.Printf("DeleteFileMetadata %s: %v", remote.Path, errDel)
						}
						s.sendStatus(remote.Path, "Skipped (deleted in cloud)", 0, remote.Size)
						return nil
					}
					if gErr, ok := err.(*googleapi.Error); ok && gErr.Code == 403 {
						log.Printf("skipping %s: file not downloadable (%v)", remote.Path, gErr.Message)
						s.sendStatus(remote.Path, "Skipped (not downloadable)", 0, remote.Size)
						return nil
					}
					s.sendStatus(remote.Path, "Error", 0, remote.Size)
					return fmt.Errorf("download %s: %w", remote.Path, err)
				}

				// Checksum verification after download.
				if remote.MD5Checksum != "" {
					if err := verifyLocalMD5Checksum(localPath, remote.MD5Checksum); err != nil {
						log.Printf("checksum mismatch %s: %v", remote.Path, err)
						s.sendStatus(remote.Path, "Checksum mismatch", 100, remote.Size)
					}
				}

				rec := remote
				rec.SyncConfigID = config.ID
				if err := s.repo.UpdateFileMetadata(downloadCtx, &rec); err != nil {
					log.Printf("UpdateFileMetadata %s: %v", remote.Path, err)
				}
				s.sendStatus(remote.Path, "Synced", 100, remote.Size)
				return nil
			})
		}
		delete(localFiles, remote.Path)
	}
	if err := downloadGrp.Wait(); err != nil {
		// Log the first download error but continue processing uploads.
		log.Printf("download phase: %v", err)
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
		s.sendStatus(relPath, "Removed local (deleted in cloud)", 100, md.Size)
	}

	// 3) Local → nuvem: ficheiros novos (sem metadados)
	uploadGrp, uploadCtx := errgroup.WithContext(ctx)
	uploadGrp.SetLimit(4)
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
			uploadGrp.Go(func() error {
				s.sendStatus(relPath, "Uploading", 0, info.Size())
				etag, err := cloudSvc.UploadFile(uploadCtx, localFullPath, config.RemoteFolderID)
				if err != nil {
					s.sendStatus(relPath, "Error", 0, info.Size())
					return fmt.Errorf("upload %s: %w", relPath, err)
				}

				if err := s.repo.UpdateFileMetadata(uploadCtx, &domain.FileMetadata{
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
				return nil
			})
		}
	}
	if err := uploadGrp.Wait(); err != nil {
		log.Printf("upload phase: %v", err)
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

	if md == nil {
		remote := domain.FileMetadata{
			SyncConfigID: config.ID,
			Path:         filepath.Base(config.LocalPath),
			ETag:         config.RemoteFolderID,
			IsDirectory:  false,
		}
		if checksum, err := cloudSvc.GetFileChecksum(ctx, config.RemoteFolderID); err == nil {
			remote.MD5Checksum = checksum
		}
		if err := s.downloadFileWithProgress(ctx, remote, config.LocalPath, config, cloudSvc); err != nil {
			if gErr, ok := err.(*googleapi.Error); ok && gErr.Code == 404 {
				log.Printf("single file %s not found in cloud, cleaning up metadata", remote.Path)
				if md != nil {
					if errDel := s.repo.DeleteFileMetadata(ctx, config.ID, md.Path); errDel != nil {
						log.Printf("DeleteFileMetadata %s: %v", md.Path, errDel)
					}
				}
				s.sendStatus(remote.Path, "Skipped (deleted in cloud)", 0, 0)
				return nil
			}
			if gErr, ok := err.(*googleapi.Error); ok && gErr.Code == 403 {
				log.Printf("single file %s not downloadable: %v", remote.Path, gErr.Message)
				s.sendStatus(remote.Path, "Skipped (not downloadable)", 0, 0)
				return nil
			}
			s.sendStatus(remote.Path, "Error", 0, 0)
			return fmt.Errorf("download single file %s: %w", remote.Path, err)
		}
		info, statErr := os.Stat(config.LocalPath)
		if statErr == nil {
			remote.Size = info.Size()
			remote.LastModified = info.ModTime()
		}
		if err := s.repo.UpdateFileMetadata(ctx, &remote); err != nil {
			log.Printf("UpdateFileMetadata single file %s: %v", remote.Path, err)
		}
		s.sendStatus(remote.Path, "Synced", 100, remote.Size)
		return nil
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

// verifyLocalMD5Checksum computes the MD5 hash of the file at localPath and
// compares it against expectedHex. Returns an error on mismatch.
func verifyLocalMD5Checksum(localPath, expectedHex string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("checksum open: %w", err)
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("checksum read: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expectedHex) {
		return fmt.Errorf("MD5 mismatch: got %s, want %s", got, expectedHex)
	}
	return nil
}

func safeJoinLocal(base string, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("nome remoto inválido: %q", name)
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\\`) || filepath.Clean(name) != name {
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
