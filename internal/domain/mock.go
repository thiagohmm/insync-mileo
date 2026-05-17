package domain

import (
	"context"
	"os"
	"path/filepath"
	"sync"
)

// MockRepository implements domain.Repository for testing
type MockRepository struct {
	mu                sync.Mutex
	Accounts          map[string]*Account
	SyncConfigs       []SyncConfig
	FileMetadataMap   map[int64]map[string]*FileMetadata
	SavedAccount      *Account
	SavedSyncConfig   *SyncConfig
	SavedFileMetadata *FileMetadata
	ErrorOn           map[string]error // keyed by operation name
}

func NewMockRepository() *MockRepository {
	return &MockRepository{
		Accounts:        make(map[string]*Account),
		SyncConfigs:     make([]SyncConfig, 0),
		FileMetadataMap: make(map[int64]map[string]*FileMetadata),
		ErrorOn:         make(map[string]error),
	}
}

func (m *MockRepository) SaveAccount(_ context.Context, account *Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["SaveAccount"]; err != nil {
		return err
	}
	m.SavedAccount = account
	m.Accounts[account.ID] = &Account{
		ID:           account.ID,
		Provider:     account.Provider,
		AccessToken:  account.AccessToken,
		RefreshToken: account.RefreshToken,
		Expiry:       account.Expiry,
	}
	return nil
}

func (m *MockRepository) GetAccount(_ context.Context, id string) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["GetAccount"]; err != nil {
		return nil, err
	}
	if acc, ok := m.Accounts[id]; ok {
		return acc, nil
	}
	return nil, nil
}

func (m *MockRepository) GetLatestAccountByProvider(_ context.Context, provider Provider) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["GetLatestAccountByProvider"]; err != nil {
		return nil, err
	}
	for _, acc := range m.Accounts {
		if acc.Provider == provider {
			return acc, nil
		}
	}
	return nil, nil
}

func (m *MockRepository) DeleteAccount(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["DeleteAccount"]; err != nil {
		return err
	}
	delete(m.Accounts, id)
	return nil
}

func (m *MockRepository) SaveSyncConfig(_ context.Context, config *SyncConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["SaveSyncConfig"]; err != nil {
		return err
	}
	m.SavedSyncConfig = config
	// Check for existing config with same local_path
	for i, c := range m.SyncConfigs {
		if c.LocalPath == config.LocalPath {
			config.ID = c.ID
			m.SyncConfigs[i] = *config
			return nil
		}
	}
	config.ID = int64(len(m.SyncConfigs) + 1)
	m.SyncConfigs = append(m.SyncConfigs, *config)
	return nil
}

func (m *MockRepository) ListSyncConfigs(_ context.Context) ([]SyncConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["ListSyncConfigs"]; err != nil {
		return nil, err
	}
	result := make([]SyncConfig, len(m.SyncConfigs))
	copy(result, m.SyncConfigs)
	return result, nil
}

func (m *MockRepository) GetSyncConfigByPath(_ context.Context, path string) (*SyncConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["GetSyncConfigByPath"]; err != nil {
		return nil, err
	}
	for _, c := range m.SyncConfigs {
		if c.LocalPath == path {
			cp := c
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *MockRepository) DeleteSyncConfigByRemoteID(_ context.Context, accountID, remoteFolderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["DeleteSyncConfigByRemoteID"]; err != nil {
		return err
	}
	// Delete file metadata for matching configs
	for _, c := range m.SyncConfigs {
		if c.AccountID == accountID && c.RemoteFolderID == remoteFolderID {
			delete(m.FileMetadataMap, c.ID)
		}
	}
	// Delete the sync config itself
	for i, c := range m.SyncConfigs {
		if c.AccountID == accountID && c.RemoteFolderID == remoteFolderID {
			m.SyncConfigs = append(m.SyncConfigs[:i], m.SyncConfigs[i+1:]...)
			break
		}
	}
	return nil
}

func (m *MockRepository) UpdateFileMetadata(_ context.Context, metadata *FileMetadata) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["UpdateFileMetadata"]; err != nil {
		return err
	}
	m.SavedFileMetadata = metadata
	if m.FileMetadataMap[metadata.SyncConfigID] == nil {
		m.FileMetadataMap[metadata.SyncConfigID] = make(map[string]*FileMetadata)
	}
	m.FileMetadataMap[metadata.SyncConfigID][metadata.Path] = &FileMetadata{
		ID:           metadata.ID,
		SyncConfigID: metadata.SyncConfigID,
		Path:         metadata.Path,
		ETag:         metadata.ETag,
		Size:         metadata.Size,
		LastModified: metadata.LastModified,
		IsDirectory:  metadata.IsDirectory,
	}
	return nil
}

func (m *MockRepository) GetFileMetadata(_ context.Context, syncConfigID int64, path string) (*FileMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["GetFileMetadata"]; err != nil {
		return nil, err
	}
	if m.FileMetadataMap[syncConfigID] != nil {
		if md, ok := m.FileMetadataMap[syncConfigID][path]; ok {
			return md, nil
		}
	}
	return nil, nil
}

func (m *MockRepository) DeleteFileMetadata(_ context.Context, syncConfigID int64, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["DeleteFileMetadata"]; err != nil {
		return err
	}
	if m.FileMetadataMap[syncConfigID] != nil {
		delete(m.FileMetadataMap[syncConfigID], path)
	}
	return nil
}

func (m *MockRepository) ListFileMetadata(_ context.Context, syncConfigID int64) ([]FileMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["ListFileMetadata"]; err != nil {
		return nil, err
	}
	metas, ok := m.FileMetadataMap[syncConfigID]
	if !ok {
		return []FileMetadata{}, nil
	}
	result := make([]FileMetadata, 0, len(metas))
	for _, md := range metas {
		result = append(result, *md)
	}
	return result, nil
}

// MockCloudService implements CloudService for testing
type MockCloudService struct {
	Provider      Provider
	Files         []FileMetadata
	UploadResult  string
	UploadError   error
	DownloadError error
	DeleteError   error
	ListError     error
}

func NewMockCloudService(provider Provider) *MockCloudService {
	return &MockCloudService{
		Provider: provider,
		Files:    make([]FileMetadata, 0),
	}
}

func (m *MockCloudService) GetProvider() Provider {
	return m.Provider
}

func (m *MockCloudService) ListFiles(_ context.Context, _ string) ([]FileMetadata, error) {
	if m.ListError != nil {
		return nil, m.ListError
	}
	result := make([]FileMetadata, len(m.Files))
	copy(result, m.Files)
	return result, nil
}

func (m *MockCloudService) UploadFile(_ context.Context, _ string, _ string) (string, error) {
	if m.UploadError != nil {
		return "", m.UploadError
	}
	return m.UploadResult, nil
}

func (m *MockCloudService) DownloadFile(_ context.Context, _ string, localPath string) error {
	if m.DownloadError != nil {
		return m.DownloadError
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(localPath, []byte("downloaded"), 0644)
}

func (m *MockCloudService) DownloadFileWithProgress(_ context.Context, _ string, localPath string, onProgress func(downloaded, total int64)) error {
	if m.DownloadError != nil {
		return m.DownloadError
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(localPath, []byte("downloaded"), 0644); err != nil {
		return err
	}
	// Simulate progress callback
	if onProgress != nil {
		onProgress(100, 100)
	}
	return nil
}

func (m *MockCloudService) DeleteFile(_ context.Context, _ string) error {
	return m.DeleteError
}

// MockSyncUseCase implements SyncUseCase for testing
type MockSyncUseCase struct {
	SyncedConfigs []SyncConfig
	SyncError     error
	StatusChan    chan SyncStatus
}

func NewMockSyncUseCase() *MockSyncUseCase {
	return &MockSyncUseCase{
		SyncedConfigs: make([]SyncConfig, 0),
		StatusChan:    make(chan SyncStatus, 100),
	}
}

func (m *MockSyncUseCase) SyncAll(_ context.Context) error {
	if m.SyncError != nil {
		return m.SyncError
	}
	return nil
}

func (m *MockSyncUseCase) SyncFolder(_ context.Context, config SyncConfig) error {
	if m.SyncError != nil {
		return m.SyncError
	}
	m.SyncedConfigs = append(m.SyncedConfigs, config)
	return nil
}

func (m *MockSyncUseCase) Statuses() <-chan SyncStatus {
	return m.StatusChan
}
