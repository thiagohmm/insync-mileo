# Correção de erros de sync: Google Docs export + 404 handling

## Problemas

1. **403 `fileNotDownloadable`** — Arquivos Google Docs (Document/Sheet/etc.) falham no poller porque `DownloadFileWithProgress` usa `Files.Get().Download()` direto. A lógica de `Files.Export()` só existe no sync inicial (`server.go:656`), não no adapter.

2. **404 `notFound`** — Arquivos deletados do Drive mas com metadata stale. O `ListFiles` retorna o arquivo, mas o download falha com 404.

## Mudanças necessárias

### 1. `internal/domain/interfaces.go`

Adicionar método `GetFileMimeType` à interface `CloudService` (line 53):

```go
// Antes (line 52-54):
	GetFileChecksum(ctx context.Context, remoteFileID string) (string, error)
}

// Depois:
	GetFileChecksum(ctx context.Context, remoteFileID string) (string, error)
	GetFileMimeType(ctx context.Context, remoteFileID string) (string, error)
}
```

### 2. `internal/domain/mock.go`

Adicionar campo e método ao `MockCloudService`:

No struct (line 425-435), adicionar campo:
```go
type MockCloudService struct {
	// ... campos existentes ...
	MimeType      string   // novo campo
}
```

Adicionar método (após `GetFileChecksum`, line 508):
```go
func (m *MockCloudService) GetFileMimeType(_ context.Context, remoteFileID string) (string, error) {
	if m.MimeType != "" {
		return m.MimeType, nil
	}
	for _, file := range m.Files {
		if file.ETag == remoteFileID {
			return file.MimeType, nil
		}
	}
	return "", nil
}
```

Adicionar `MimeType` ao `FileMetadata` no mock (se necessário, ou usar campo existente).

### 3. `internal/adapters/cloud/google_drive.go`

#### 3a. Adicionar método `GetFileMimeType` (após `GetFileChecksum`, line 163):

```go
func (g *googleDriveService) GetFileMimeType(ctx context.Context, remoteFileID string) (string, error) {
	file, err := g.service.Files.Get(remoteFileID).Fields("mimeType").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return file.MimeType, nil
}
```

#### 3b. Refatorar `DownloadFile` (line 81-96) para usar export:

```go
func (g *googleDriveService) DownloadFile(ctx context.Context, remoteFileID string, localPath string) error {
	mimeType, err := g.GetFileMimeType(ctx, remoteFileID)
	if err != nil {
		return err
	}

	var body io.ReadCloser
	if strings.HasPrefix(mimeType, "application/vnd.google-apps.") {
		exportMime := googleExportMime(mimeType)
		res, err := g.service.Files.Export(remoteFileID, exportMime).Download()
		if err != nil {
			return err
		}
		body = res.Body
		localPath = ensureExportExtension(localPath, exportMime)
	} else {
		res, err := g.service.Files.Get(remoteFileID).Download()
		if err != nil {
			return err
		}
		body = res.Body
	}
	defer body.Close()

	out, err := secureCreateLocalFile(localPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, body)
	return err
}
```

#### 3c. Refatorar `DownloadFileWithProgress` (line 98-132) para usar export:

```go
func (g *googleDriveService) DownloadFileWithProgress(ctx context.Context, remoteFileID string, localPath string, onProgress func(downloaded, total int64)) error {
	mimeType, err := g.GetFileMimeType(ctx, remoteFileID)
	if err != nil {
		return err
	}

	var body io.ReadCloser
	var totalSize int64

	if strings.HasPrefix(mimeType, "application/vnd.google-apps.") {
		exportMime := googleExportMime(mimeType)
		res, err := g.service.Files.Export(remoteFileID, exportMime).Download()
		if err != nil {
			return err
		}
		body = res.Body
		localPath = ensureExportExtension(localPath, exportMime)
		if res.ContentLength > 0 {
			totalSize = res.ContentLength
		}
	} else {
		res, err := g.service.Files.Get(remoteFileID).Download()
		if err != nil {
			return err
		}
		body = res.Body
		if res.ContentLength > 0 {
			totalSize = res.ContentLength
		} else {
			meta, err := g.service.Files.Get(remoteFileID).Fields("size").Do()
			if err == nil && meta.Size > 0 {
				totalSize = meta.Size
			}
		}
	}
	defer body.Close()

	out, err := secureCreateLocalFile(localPath)
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
```

#### 3d. Mover funções `googleExportMime` e `ensureExportExtension` de `server.go` para este arquivo, ou criar em arquivo shared.

As funções existem em `server.go` (line 749-772). Mover para `google_drive.go` (final do arquivo, após line 221):

```go
func googleExportMime(mimeType string) string {
	switch mimeType {
	case "application/vnd.google-apps.spreadsheet":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "application/vnd.google-apps.presentation":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	default:
		return "application/pdf"
	}
}

func ensureExportExtension(path string, exportMime string) string {
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
```

Depois remover os duplicates de `server.go` (line 749-772) — o package `grpc` pode importar `cloud` e usar `cloud.GoogleExportMime` ou manter como alias se não houver ciclo de dependência. Na prática, o `server.go` já importa `cloud`? Verificar. Se não, as funções ficam no package `cloud` e o `server.go` pode chamá-las.

**Importante:** Verificar se `server.go` importa `cloud`. Se sim, renomear para `GoogleExportMime` e `EnsureExportExtension` (exported) e usar de lá. Se não, manter como funções separadas ou mover para um package utilitário.

### 4. `internal/usecases/sync.go`

#### 4a. Adicionar import do `googleapi`:

No bloco de imports (line 3-21), adicionar:
```go
"google.golang.org/api/googleapi"
```

#### 4b. Tratamento de erros no download phase (SyncFolder, line 138-142):

Substituir o goroutine de download (line 138-159) para tratar erros 404 e 403:

```go
downloadGrp.Go(func() error {
	if err := s.downloadFileWithProgress(downloadCtx, remote, localPath, config, cloudSvc); err != nil {
		// Treat 404 (file deleted in cloud) gracefully
		if gErr, ok := err.(*googleapi.Error); ok && gErr.Code == 404 {
			log.Printf("skipping %s: file not found in cloud (deleted)", remote.Path)
			if errDel := s.repo.DeleteFileMetadata(downloadCtx, config.ID, remote.Path); errDel != nil {
				log.Printf("DeleteFileMetadata %s: %v", remote.Path, errDel)
			}
			s.sendStatus(remote.Path, "Skipped (deleted in cloud)", 0, remote.Size)
			return nil
		}
		// Treat 403 fileNotDownloadable gracefully (Google Docs that can't be exported)
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
```

#### 4c. Tratamento em `syncSingleFile` (line 287-289):

```go
if err := s.downloadFileWithProgress(ctx, remote, config.LocalPath, config, cloudSvc); err != nil {
	// File deleted in cloud — clean up stale metadata
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
	// Google Docs not downloadable
	if gErr, ok := err.(*googleapi.Error); ok && gErr.Code == 403 {
		log.Printf("single file %s not downloadable: %v", remote.Path, gErr.Message)
		s.sendStatus(remote.Path, "Skipped (not downloadable)", 0, 0)
		return nil
	}
	s.sendStatus(remote.Path, "Error", 0, 0)
	return fmt.Errorf("download single file %s: %w", remote.Path, err)
}
```

### 5. `internal/infrastructure/grpc/server.go`

Atualizar chamadas a `googleExportMime` e `ensureExportExtension` (line 657, 663, 749-772) para usar as funções do package `cloud`:

```go
// Line 657:
exportMime := cloud.GoogleExportMime(meta.MimeType)
// Line 663:
localPath = cloud.EnsureExportExtension(localPath, exportMime)
```

E remover as funções duplicadas (line 749-772).

**OU**, se preferir evitar exportar funções do package `cloud`, manter as cópias em `server.go`. Isso é mais simples e evita acoplamento.

---

## Novos testes

### `internal/adapters/cloud/google_drive_test.go` — Adicionar:

```go
func TestGoogleExportMime(t *testing.T) {
	tests := []struct {
		input  string
		output string
	}{
		{"application/vnd.google-apps.document", "application/pdf"},
		{"application/vnd.google-apps.spreadsheet", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{"application/vnd.google-apps.presentation", "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
		{"application/vnd.google-apps.drawing", "application/pdf"},
		{"application/vnd.google-apps.form", "application/pdf"},
	}
	for _, tt := range tests {
		got := googleExportMime(tt.input)
		if got != tt.output {
			t.Errorf("googleExportMime(%q) = %q, want %q", tt.input, got, tt.output)
		}
	}
}

func TestEnsureExportExtension(t *testing.T) {
	tests := []struct {
		path   string
		mime   string
		expect string
	}{
		{"documento", "application/pdf", "documento.pdf"},
		{"planilha", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "planilha.xlsx"},
		{"apresentacao", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "apresentacao.pptx"},
		{"arquivo.txt", "application/pdf", "arquivo.txt"}, // já tem extensão
	}
	for _, tt := range tests {
		got := ensureExportExtension(tt.path, tt.mime)
		if got != tt.expect {
			t.Errorf("ensureExportExtension(%q, %q) = %q, want %q", tt.path, tt.mime, got, tt.expect)
		}
	}
}
```

### `internal/usecases/sync_test.go` — Adicionar:

```go
func TestSyncUseCase_Download404Error_RemovesStaleMetadata(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	// Create 404 error
	apiErr := &googleapi.Error{Code: 404, Message: "File not found"}
	cloudSvc.DownloadError = apiErr

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-404")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-404",
		LocalPath:      localPath,
		RemoteFolderID: "remote-404",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Remote has a file but download will fail with 404
	cloudSvc.Files = []domain.FileMetadata{
		{Path: "deleted-file.txt", ETag: "etag-deleted", Size: 100, IsDirectory: false},
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() should not return error for 404: %v", err)
	}
}

func TestSyncUseCase_Download403Error_SkipsGracefully(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	apiErr := &googleapi.Error{Code: 403, Message: "fileNotDownloadable"}
	cloudSvc.DownloadError = apiErr

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "sync-403")
	os.MkdirAll(localPath, 0755)

	cfg := &domain.SyncConfig{
		AccountID:      "acct-403",
		LocalPath:      localPath,
		RemoteFolderID: "remote-403",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    true,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	cloudSvc.Files = []domain.FileMetadata{
		{Path: "google-doc.txt", ETag: "etag-docs", Size: 100, IsDirectory: false},
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() should not return error for 403: %v", err)
	}
}

func TestSyncUseCase_SyncSingleFile_404_RemovesMetadata(t *testing.T) {
	repo := domain.NewMockRepository()
	cloudSvc := domain.NewMockCloudService(domain.GoogleDrive)

	apiErr := &googleapi.Error{Code: 404, Message: "File not found"}
	cloudSvc.DownloadError = apiErr

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "single-404.txt")

	cfg := &domain.SyncConfig{
		AccountID:      "acct-single-404",
		LocalPath:      localPath,
		RemoteFolderID: "remote-file-404",
		Mode:           domain.BaseSync,
		Provider:       domain.GoogleDrive,
		IsDirectory:    false,
	}
	if err := repo.SaveSyncConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveSyncConfig() error: %v", err)
	}

	// Pre-existing metadata
	if err := repo.UpdateFileMetadata(context.Background(), &domain.FileMetadata{
		SyncConfigID: cfg.ID,
		Path:         "single-404.txt",
		ETag:         "remote-file-404",
		Size:         100,
	}); err != nil {
		t.Fatalf("UpdateFileMetadata() error: %v", err)
	}

	suc := NewSyncUseCase(repo, []domain.CloudService{cloudSvc})
	err := suc.SyncFolder(context.Background(), *cfg)
	if err != nil {
		t.Errorf("SyncFolder() should not return error for 404 single file: %v", err)
	}
}
```

### `internal/domain/mock.go` — Adicionar campo `MimeType` ao `FileMetadata`:

No `FileMetadata` entity (`entities.go`), adicionar campo `MimeType string`. Ou usar o campo existente sem alterar a entity — o mock pode usar um mapa separada.

---

## Ordem de execução

1. Adicionar `MimeType` ao `FileMetadata` entity (se necessário)
2. Adicionar `GetFileMimeType` à interface + mock
3. Implementar `GetFileMimeType` no `google_drive.go`
4. Mover `googleExportMime` + `ensureExportExtension` para `google_drive.go` (ou exportar como `GoogleExportMime`)
5. Refatorar `DownloadFile` e `DownloadFileWithProgress` em `google_drive.go`
6. Atualizar `server.go` para usar funções compartilhadas
7. Tratar erros 404/403 em `sync.go`
8. Rodar `go test ./...`
9. Adicionar novos testes
10. Rodar `go test ./...` novamente
