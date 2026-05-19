package cloud

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	q := driveParentQuery(folderID)
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
			MD5Checksum:  f.Md5Checksum,
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
	body, path, err := g.downloadBody(ctx, remoteFileID, localPath)
	if err != nil {
		return err
	}
	defer body.Close()

	out, err := secureCreateLocalFile(path)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, body)
	return err
}

func (g *googleDriveService) DownloadFileWithProgress(ctx context.Context, remoteFileID string, localPath string, onProgress func(downloaded, total int64)) error {
	body, path, err := g.downloadBody(ctx, remoteFileID, localPath)
	if err != nil {
		return err
	}
	defer body.Close()

	// Get total size from metadata fallback
	var totalSize int64
	meta, err := g.service.Files.Get(remoteFileID).Fields("size").Do()
	if err == nil && meta.Size > 0 {
		totalSize = meta.Size
	}

	out, err := secureCreateLocalFile(path)
	if err != nil {
		return err
	}
	defer out.Close()

	wrapped := &progressReader{
		reader:     body,
		total:      totalSize,
		onProgress: onProgress,
	}

	_, err = io.Copy(out, wrapped)
	return err
}

// progressReader wraps an io.Reader to report progress
type progressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	onProgress func(downloaded, total int64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.reader.Read(buf)
	if n > 0 {
		p.downloaded += int64(n)
		if p.total > 0 && p.onProgress != nil {
			p.onProgress(p.downloaded, p.total)
		}
	}
	return n, err
}

func (g *googleDriveService) DeleteFile(ctx context.Context, remoteFileID string) error {
	return g.service.Files.Delete(remoteFileID).Do()
}

func (g *googleDriveService) GetFileChecksum(ctx context.Context, remoteFileID string) (string, error) {
	file, err := g.service.Files.Get(remoteFileID).Fields("md5Checksum").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return file.Md5Checksum, nil
}

func (g *googleDriveService) GetFileMimeType(ctx context.Context, remoteFileID string) (string, error) {
	file, err := g.service.Files.Get(remoteFileID).Fields("mimeType").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return file.MimeType, nil
}

// downloadBody fetches the file body from Drive, using Export for Google Docs files.
// It returns the body reader, the (possibly adjusted) local path, and any error.
func (g *googleDriveService) downloadBody(ctx context.Context, remoteFileID string, localPath string) (io.ReadCloser, string, error) {
	mimeType, err := g.GetFileMimeType(ctx, remoteFileID)
	if err != nil {
		return nil, "", err
	}

	if strings.HasPrefix(mimeType, "application/vnd.google-apps.") {
		exportMime := GoogleExportMime(mimeType)
		res, err := g.service.Files.Export(remoteFileID, exportMime).Context(ctx).Download()
		if err != nil {
			return nil, "", err
		}
		return res.Body, EnsureExportExtension(localPath, exportMime), nil
	}

	res, err := g.service.Files.Get(remoteFileID).Context(ctx).Download()
	if err != nil {
		return nil, "", err
	}
	return res.Body, localPath, nil
}

func driveParentQuery(folderID string) string {
	return fmt.Sprintf("'%s' in parents and trashed = false", escapeDriveQueryString(folderID))
}

func escapeDriveQueryString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `'`, `\'`)
	return value
}

func secureCreateLocalFile(path string) (*os.File, error) {
	if err := ensurePathHasNoSymlink(path); err != nil {
		return nil, err
	}
	if err := ensurePathHasNoSymlink(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("caminho local inseguro: %s é symlink", path)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
}

func ensurePathHasNoSymlink(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	clean := filepath.Clean(abs)
	volume := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, volume)
	rest = strings.TrimPrefix(rest, string(filepath.Separator))

	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("caminho local inseguro: %s é symlink", current)
		}
	}
	return nil
}

// GoogleExportMime returns the appropriate export MIME type for a Google Docs file.
func GoogleExportMime(mimeType string) string {
	switch mimeType {
	case "application/vnd.google-apps.spreadsheet":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "application/vnd.google-apps.presentation":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	default:
		return "application/pdf"
	}
}

// EnsureExportExtension ensures the local path has the correct extension for the export MIME type.
func EnsureExportExtension(path string, exportMime string) string {
	if filepath.Ext(path) != "" {
		return path
	}
	switch exportMime {
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return path + ".xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return path + ".pptx"
	default:
		return path + ".pdf"
	}
}
