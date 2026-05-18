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
	DedupKeys         map[string]*FileDeduplicationKey
	SyncTasks         []SyncTask
	WebhookConfigs    []WebhookConfig
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
		DedupKeys:       make(map[string]*FileDeduplicationKey),
		SyncTasks:       make([]SyncTask, 0),
		WebhookConfigs:  make([]WebhookConfig, 0),
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
		MD5Checksum:  metadata.MD5Checksum,
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

func (m *MockRepository) SaveDedupKey(_ context.Context, key *FileDeduplicationKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["SaveDedupKey"]; err != nil {
		return err
	}
	m.DedupKeys[key.MD5Checksum] = &FileDeduplicationKey{
		MD5Checksum: key.MD5Checksum,
		Size:        key.Size,
	}
	return nil
}

func (m *MockRepository) GetDedupKey(_ context.Context, md5 string, size int64) (*FileDeduplicationKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["GetDedupKey"]; err != nil {
		return nil, err
	}
	key, ok := m.DedupKeys[md5]
	if !ok || key.Size != size {
		return nil, nil
	}
	return key, nil
}

func (m *MockRepository) ListFilesByDedupKey(_ context.Context, key *FileDeduplicationKey) ([]FileMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["ListFilesByDedupKey"]; err != nil {
		return nil, err
	}
	var result []FileMetadata
	for _, metas := range m.FileMetadataMap {
		for _, md := range metas {
			if md.MD5Checksum == key.MD5Checksum && md.Size == key.Size {
				result = append(result, *md)
			}
		}
	}
	return result, nil
}

func (m *MockRepository) SaveSyncTask(_ context.Context, task *SyncTask) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["SaveSyncTask"]; err != nil {
		return err
	}
	if task.ID == 0 {
		task.ID = int64(len(m.SyncTasks) + 1)
	}
	m.SyncTasks = append(m.SyncTasks, *task)
	return nil
}

func (m *MockRepository) GetNextSyncTask(_ context.Context, limit int) ([]SyncTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["GetNextSyncTask"]; err != nil {
		return nil, err
	}
	if limit <= 0 || limit > len(m.SyncTasks) {
		limit = len(m.SyncTasks)
	}
	result := make([]SyncTask, limit)
	copy(result, m.SyncTasks[:limit])
	return result, nil
}

func (m *MockRepository) UpdateSyncTask(_ context.Context, task *SyncTask) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["UpdateSyncTask"]; err != nil {
		return err
	}
	for i := range m.SyncTasks {
		if m.SyncTasks[i].ID == task.ID {
			m.SyncTasks[i] = *task
			return nil
		}
	}
	m.SyncTasks = append(m.SyncTasks, *task)
	return nil
}

func (m *MockRepository) DeleteSyncTask(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["DeleteSyncTask"]; err != nil {
		return err
	}
	for i := range m.SyncTasks {
		if m.SyncTasks[i].ID == id {
			m.SyncTasks = append(m.SyncTasks[:i], m.SyncTasks[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *MockRepository) ListSyncTasksByConfig(_ context.Context, configID int64) ([]SyncTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["ListSyncTasksByConfig"]; err != nil {
		return nil, err
	}
	var result []SyncTask
	for _, task := range m.SyncTasks {
		if task.SyncConfigID == configID {
			result = append(result, task)
		}
	}
	return result, nil
}

func (m *MockRepository) SaveWebhookConfig(_ context.Context, config *WebhookConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["SaveWebhookConfig"]; err != nil {
		return err
	}
	if config.ID == 0 {
		config.ID = int64(len(m.WebhookConfigs) + 1)
	}
	m.WebhookConfigs = append(m.WebhookConfigs, *config)
	return nil
}

func (m *MockRepository) GetWebhookConfig(_ context.Context, syncConfigID int64) (*WebhookConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["GetWebhookConfig"]; err != nil {
		return nil, err
	}
	for _, config := range m.WebhookConfigs {
		if config.SyncConfigID == syncConfigID {
			cp := config
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *MockRepository) ListWebhookConfigs(_ context.Context) ([]WebhookConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["ListWebhookConfigs"]; err != nil {
		return nil, err
	}
	result := make([]WebhookConfig, len(m.WebhookConfigs))
	copy(result, m.WebhookConfigs)
	return result, nil
}

func (m *MockRepository) DeleteWebhookConfig(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ErrorOn["DeleteWebhookConfig"]; err != nil {
		return err
	}
	for i := range m.WebhookConfigs {
		if m.WebhookConfigs[i].ID == id {
			m.WebhookConfigs = append(m.WebhookConfigs[:i], m.WebhookConfigs[i+1:]...)
			return nil
		}
	}
	return nil
}

// MockCloudService implements CloudService for testing
type MockCloudService struct {
	Provider      Provider
	Files         []FileMetadata
	UploadResult  string
	Checksum      string
	UploadError   error
	DownloadError error
	DeleteError   error
	ListError     error
	ChecksumError error
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

func (m *MockCloudService) GetFileChecksum(_ context.Context, remoteFileID string) (string, error) {
	if m.ChecksumError != nil {
		return "", m.ChecksumError
	}
	if m.Checksum != "" {
		return m.Checksum, nil
	}
	for _, file := range m.Files {
		if file.ETag == remoteFileID {
			return file.MD5Checksum, nil
		}
	}
	return "", nil
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

func (m *MockSyncUseCase) ProcessTaskQueue(_ context.Context) error {
	return m.SyncError
}

func (m *MockSyncUseCase) HandleWebhookNotification(_ *PushNotification) error {
	return m.SyncError
}

func (m *MockSyncUseCase) RegisterWebhook(_ context.Context, config SyncConfig) error {
	if m.SyncError != nil {
		return m.SyncError
	}
	m.SyncedConfigs = append(m.SyncedConfigs, config)
	return nil
}

func (m *MockSyncUseCase) ComputeAndStoreChecksum(_ context.Context, _ string) (string, error) {
	if m.SyncError != nil {
		return "", m.SyncError
	}
	return "", nil
}
