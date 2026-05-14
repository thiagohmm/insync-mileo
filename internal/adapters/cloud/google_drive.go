package cloud

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
	"google.golang.org/api/drive/v3"
)

type googleDriveService struct {
	service *drive.Service
}

func NewGoogleDriveService(service *drive.Service) domain.CloudService {
	return &googleDriveService{service: service}
}

func (g *googleDriveService) GetProvider() domain.Provider {
	return domain.GoogleDrive
}

func (g *googleDriveService) ListFiles(ctx context.Context, folderID string) ([]domain.FileMetadata, error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
	call := g.service.Files.List().Q(q).Fields("files(id, name, size, md5Checksum, modifiedTime, mimeType)")
	res, err := call.Do()
	if err != nil {
		return nil, err
	}

	var files []domain.FileMetadata
	for _, f := range res.Files {
		var mod time.Time
		if f.ModifiedTime != "" {
			mod, _ = time.Parse(time.RFC3339, f.ModifiedTime)
		}
		files = append(files, domain.FileMetadata{
			Path:         f.Name,
			ETag:         f.Id, // ID remoto usado em Download/Upload no Drive
			Size:         f.Size,
			LastModified: mod,
			IsDirectory:  f.MimeType == "application/vnd.google-apps.folder",
		})
	}
	return files, nil
}

func (g *googleDriveService) UploadFile(ctx context.Context, localPath string, remoteFolderID string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	driveFile := &drive.File{
		Name:    info.Name(),
		Parents: []string{remoteFolderID},
	}

	// Use Resumable Upload for large files or simply media upload for small ones.
	// For this exercise, I'll use the standard media upload which supports streaming.
	res, err := g.service.Files.Create(driveFile).Media(f).Do()
	if err != nil {
		return "", err
	}

	return res.Id, nil
}

func (g *googleDriveService) DownloadFile(ctx context.Context, remoteFileID string, localPath string) error {
	res, err := g.service.Files.Get(remoteFileID).Download()
	if err != nil {
		return err
	}
	defer res.Body.Close()

	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, res.Body)
	return err
}

func (g *googleDriveService) DeleteFile(ctx context.Context, remoteFileID string) error {
	return g.service.Files.Delete(remoteFileID).Do()
}
