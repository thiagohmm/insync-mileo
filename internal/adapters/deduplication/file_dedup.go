package deduplication

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"os"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

type FileDeduplicationService struct {
	repo domain.Repository
}

func NewFileDeduplicationService(repo domain.Repository) *FileDeduplicationService {
	return &FileDeduplicationService{repo: repo}
}

func (s *FileDeduplicationService) ComputeChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (s *FileDeduplicationService) StoreDedupKey(ctx context.Context, key *domain.FileDeduplicationKey) error {
	return s.repo.SaveDedupKey(ctx, key)
}

func (s *FileDeduplicationService) GetExistingFile(ctx context.Context, key *domain.FileDeduplicationKey) (*domain.FileMetadata, error) {
	files, err := s.repo.ListFilesByDedupKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	return &files[0], nil
}

func (s *FileDeduplicationService) IsDuplicate(ctx context.Context, key *domain.FileDeduplicationKey) (bool, error) {
	existing, err := s.GetExistingFile(ctx, key)
	if err != nil {
		return false, err
	}
	return existing != nil, nil
}

var _ domain.DeduplicationService = (*FileDeduplicationService)(nil)
