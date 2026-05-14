package cloud

import (
	"context"
	"net/http"
	"os"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

type oneDriveService struct {
	client *http.Client
}

func NewOneDriveService(client *http.Client) domain.CloudService {
	return &oneDriveService{client: client}
}

func (o *oneDriveService) GetProvider() domain.Provider {
	return domain.OneDrive
}

func (o *oneDriveService) ListFiles(ctx context.Context, folderID string) ([]domain.FileMetadata, error) {
	// Implementation using Microsoft Graph API
	// GET /me/drive/items/{folderID}/children
	return nil, nil
}

func (o *oneDriveService) UploadFile(ctx context.Context, localPath string, remoteFolderID string) (string, error) {
	// For OneDrive large files:
	// 1. Create upload session: POST /me/drive/items/{folderID}:/{filename}:/createUploadSession
	// 2. Upload chunks of 2MB (must be multiple of 320KB)
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Placeholder for the actual implementation
	return "onedrive-id", nil
}

func (o *oneDriveService) DownloadFile(ctx context.Context, remoteFileID string, localPath string) error {
	// GET /me/drive/items/{remoteFileID}/content
	return nil
}

func (o *oneDriveService) DownloadFileWithProgress(ctx context.Context, remoteFileID string, localPath string, onProgress func(downloaded, total int64)) error {
	// GET /me/drive/items/{remoteFileID}/content
	// For now, just call DownloadFile since we don't have a real implementation
	return nil
}

func (o *oneDriveService) DeleteFile(ctx context.Context, remoteFileID string) error {
	// DELETE /me/drive/items/{remoteFileID}
	return nil
}
