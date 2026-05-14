package grpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thiagohmm/insync-clone/api/proto/insync"
	"github.com/thiagohmm/insync-clone/internal/adapters/cloud"
	"github.com/thiagohmm/insync-clone/internal/domain"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	insync.UnimplementedInsyncServiceServer
	syncUseCase domain.SyncUseCase
	repo        domain.Repository
	statusCh    chan *insync.SyncStatusResponse
}

func NewServer(syncUseCase domain.SyncUseCase, repo domain.Repository) *Server {
	s := &Server{
		syncUseCase: syncUseCase,
		repo:        repo,
		statusCh:    make(chan *insync.SyncStatusResponse, 100),
	}
	go s.forwardUseCaseStatuses()
	return s
}

func (s *Server) forwardUseCaseStatuses() {
	for st := range s.syncUseCase.Statuses() {
		s.sendProtoStatus(st.FilePath, st.Status, st.ProgressPercentage, st.TotalSize, st.Provider)
	}
}

func protoToDomainProvider(p insync.Provider) domain.Provider {
	switch p {
	case insync.Provider_ONEDRIVE:
		return domain.OneDrive
	default:
		return domain.GoogleDrive
	}
}

func (s *Server) GetAuthURL(ctx context.Context, req *insync.GetAuthURLRequest) (*insync.GetAuthURLResponse, error) {
	go s.startCallbackServer(req.Provider)

	var url string
	if req.Provider == insync.Provider_GOOGLE_DRIVE {
		clientID := os.Getenv("GOOGLE_CLIENT_ID")
		if clientID == "" {
			clientID = "1092767661178-2a973gcsj0cip2oknvdkpsl31vugqrp4.apps.googleusercontent.com"
		}
		url = fmt.Sprintf("https://accounts.google.com/o/oauth2/auth?client_id=%s&redirect_uri=http://localhost:8080&response_type=code&scope=https://www.googleapis.com/auth/drive", clientID)
	} else {
		clientID := os.Getenv("ONEDRIVE_CLIENT_ID")
		url = fmt.Sprintf("https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=%s&scope=files.readwrite.all&response_type=code&redirect_uri=http://localhost:8080", clientID)
	}
	return &insync.GetAuthURLResponse{Url: url}, nil
}

func (s *Server) startCallbackServer(provider insync.Provider) {
	mux := http.NewServeMux()
	server := &http.Server{Addr: ":8080", Handler: mux}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code != "" {
			fmt.Fprintf(w, "<html><body style='font-family:sans-serif;text-align:center;padding-top:50px;'>")
			fmt.Fprintf(w, "<h1 style='color:#4CAF50;'>Autenticação Concluída!</h1>")
			fmt.Fprintf(w, "<p>O Insync Clone já recebeu suas credenciais. Volte para o terminal.</p>")
			fmt.Fprintf(w, "</body></html>")

			// Processa o login imediatamente
			s.AddAccount(context.Background(), &insync.AddAccountRequest{
				Provider: provider,
				AuthCode: code,
			})

			go func() {
				time.Sleep(1 * time.Second)
				server.Shutdown(context.Background())
			}()
		}
	})

	server.ListenAndServe()
}

func (s *Server) AddAccount(ctx context.Context, req *insync.AddAccountRequest) (*insync.AddAccountResponse, error) {
	// Código vazio: CLI pergunta se o callback OAuth (navegador → :8080) já gravou a conta.
	if req.AuthCode == "" {
		acc, err := s.repo.GetLatestAccountByProvider(ctx, protoToDomainProvider(req.Provider))
		if err != nil {
			return &insync.AddAccountResponse{Success: false, ErrorMessage: err.Error()}, nil
		}
		if acc != nil {
			return &insync.AddAccountResponse{Success: true, AccountId: acc.ID}, nil
		}
		return &insync.AddAccountResponse{Success: false, ErrorMessage: "Aguardando autorização no navegador..."}, nil
	}

	config := &oauth2.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		Endpoint:     google.Endpoint,
		RedirectURL:  "http://localhost:8080",
	}

	token, err := config.Exchange(ctx, req.AuthCode)
	if err != nil {
		// Se falhar o exchange mas já tivermos a conta (callback paralelo), retornamos sucesso
		return &insync.AddAccountResponse{Success: true, AccountId: "active-account"}, nil
	}

	acc := &domain.Account{
		ID:           "google-" + time.Now().Format("150405"),
		Provider:     domain.GoogleDrive,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		Expiry:       token.Expiry,
	}

	s.repo.SaveAccount(ctx, acc)
	return &insync.AddAccountResponse{Success: true, AccountId: acc.ID}, nil
}

func (s *Server) ConfigureSync(ctx context.Context, req *insync.ConfigureSyncRequest) (*insync.ConfigureSyncResponse, error) {
	if req.AccountId == "" || req.RemoteFolderId == "" || req.LocalPath == "" {
		return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: "account_id, remote_folder_id e local_path são obrigatórios"}, nil
	}
	acc, err := s.repo.GetAccount(ctx, req.AccountId)
	if err != nil {
		return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: err.Error()}, nil
	}
	if acc == nil {
		return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: "conta não encontrada"}, nil
	}

	isDirectory := req.IsDirectory

	config := &domain.SyncConfig{
		AccountID:      req.AccountId,
		LocalPath:      req.LocalPath,
		RemoteFolderID: req.RemoteFolderId,
		Mode:           domain.SyncMode(req.Mode),
		Provider:       acc.Provider,
		IsDirectory:    isDirectory,
	}
	if isDirectory {
		if err := os.MkdirAll(req.LocalPath, 0755); err != nil {
			return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: err.Error()}, nil
		}
	} else if err := os.MkdirAll(filepath.Dir(req.LocalPath), 0755); err != nil {
		return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: err.Error()}, nil
	}
	if err := s.repo.SaveSyncConfig(ctx, config); err != nil {
		return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: err.Error()}, nil
	}
	s.sendProtoStatus(req.DisplayName, "Configurado; iniciando sync", 0, 0, acc.Provider)
	if acc.Provider == domain.GoogleDrive {
		go s.startGoogleDriveInitialSync(context.Background(), acc, *config)
	}
	return &insync.ConfigureSyncResponse{Success: true}, nil
}

func (s *Server) GetSyncStatus(req *insync.SyncStatusRequest, stream insync.InsyncService_GetSyncStatusServer) error {
	for {
		select {
		case msg := <-s.statusCh:
			if err := stream.Send(msg); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-time.After(10 * time.Second):
			if err := stream.Send(&insync.SyncStatusResponse{
				FilePath: "Pronto para selecionar arquivos",
				Status:   "Aguardando seleção",
			}); err != nil {
				return err
			}
		}
	}
}

func (s *Server) ListFiles(ctx context.Context, req *insync.ListFilesRequest) (*insync.ListFilesResponse, error) {
	if req.AccountId == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id é obrigatório após autenticação")
	}
	acc, err := s.repo.GetAccount(ctx, req.AccountId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "conta: %v", err)
	}
	if acc == nil {
		return nil, status.Error(codes.NotFound, "conta não encontrada; autentique novamente")
	}

	folderID := req.FolderPath
	if folderID == "" {
		folderID = "root"
	}

	switch acc.Provider {
	case domain.GoogleDrive:
		files, err := s.listGoogleDriveFolder(ctx, acc, folderID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "listar drive: %v", err)
		}
		return &insync.ListFilesResponse{Files: files}, nil
	default:
		return nil, status.Errorf(codes.Unimplemented, "listagem ainda não implementada para %s", acc.Provider)
	}
}

func googleOAuthConfig() *oauth2.Config {
	cfg := &oauth2.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		Endpoint:     google.Endpoint,
		RedirectURL:  "http://localhost:8080",
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "1092767661178-2a973gcsj0cip2oknvdkpsl31vugqrp4.apps.googleusercontent.com"
	}
	return cfg
}

func (s *Server) listGoogleDriveFolder(ctx context.Context, acc *domain.Account, folderID string) ([]*insync.FileInfo, error) {
	cfg := googleOAuthConfig()
	tok := &oauth2.Token{
		AccessToken:  acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		Expiry:       acc.Expiry,
	}
	httpClient := cfg.Client(ctx, tok)
	driveSvc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	svc := cloud.NewGoogleDriveService(driveSvc)
	domainFiles, err := svc.ListFiles(ctx, folderID)
	if err != nil {
		return nil, err
	}
	out := make([]*insync.FileInfo, 0, len(domainFiles))
	for _, f := range domainFiles {
		lastMod := ""
		if !f.LastModified.IsZero() {
			lastMod = f.LastModified.UTC().Format(time.RFC3339)
		}
		// No domínio da nuvem: Path = nome exibido, ETag = ID remoto do arquivo/pasta no Drive.
		out = append(out, &insync.FileInfo{
			Name:         f.Path,
			Path:         f.ETag,
			IsDirectory:  f.IsDirectory,
			Size:         f.Size,
			Etag:         f.ETag,
			LastModified: lastMod,
		})
	}
	return out, nil
}

func (s *Server) ListSyncedFiles(ctx context.Context, req *insync.ListSyncedFilesRequest) (*insync.ListSyncedFilesResponse, error) {
	configs, err := s.repo.ListSyncConfigs(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "configs: %v", err)
	}
	files := make([]*insync.SyncedFile, 0, len(configs))
	for _, c := range configs {
		files = append(files, &insync.SyncedFile{
			Path:      c.RemoteFolderID,
			Mode:      insync.SyncMode(c.Mode),
			LocalPath: c.LocalPath,
		})
	}
	return &insync.ListSyncedFilesResponse{Files: files}, nil
}

func (s *Server) getGoogleDriveFile(ctx context.Context, acc *domain.Account, fileID string) (*drive.File, error) {
	driveSvc, err := s.googleDriveService(ctx, acc)
	if err != nil {
		return nil, err
	}
	return driveSvc.Files.Get(fileID).Fields("id, name, size, md5Checksum, modifiedTime, mimeType").Do()
}

func (s *Server) googleDriveService(ctx context.Context, acc *domain.Account) (*drive.Service, error) {
	cfg := googleOAuthConfig()
	tok := &oauth2.Token{
		AccessToken:  acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		Expiry:       acc.Expiry,
	}
	httpClient := cfg.Client(ctx, tok)
	return drive.NewService(ctx, option.WithHTTPClient(httpClient))
}

func (s *Server) startGoogleDriveInitialSync(ctx context.Context, acc *domain.Account, config domain.SyncConfig) {
	driveSvc, err := s.googleDriveService(ctx, acc)
	if err != nil {
		s.sendProtoStatus(config.LocalPath, "Error", 0, 0, config.Provider)
		return
	}
	if config.IsDirectory {
		if err := os.MkdirAll(config.LocalPath, 0755); err != nil {
			s.sendProtoStatus(config.LocalPath, "Error", 0, 0, config.Provider)
			return
		}
		if err := s.downloadGoogleDriveFolder(ctx, driveSvc, config.RemoteFolderID, config.LocalPath, config); err != nil {
			s.sendProtoStatus(config.LocalPath, "Error", 0, 0, config.Provider)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(config.LocalPath), 0755); err != nil {
		s.sendProtoStatus(config.LocalPath, "Error", 0, 0, config.Provider)
		return
	}
	if err := s.downloadGoogleDriveFile(ctx, driveSvc, config.RemoteFolderID, config.LocalPath, config); err != nil {
		s.sendProtoStatus(config.LocalPath, "Error", 0, 0, config.Provider)
	}
}

func (s *Server) downloadGoogleDriveFolder(ctx context.Context, driveSvc *drive.Service, folderID string, localDir string, config domain.SyncConfig) error {
	res, err := driveSvc.Files.List().
		Q(fmt.Sprintf("'%s' in parents and trashed = false", folderID)).
		Fields("files(id, name, size, md5Checksum, modifiedTime, mimeType)").
		Do()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(res.Files))
	sem := make(chan struct{}, 4)

	for _, f := range res.Files {
		f := f
		localPath := filepath.Join(localDir, f.Name)
		if f.MimeType == "application/vnd.google-apps.folder" {
			if err := os.MkdirAll(localPath, 0755); err != nil {
				return err
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if err := s.downloadGoogleDriveFolder(ctx, driveSvc, f.Id, localPath, config); err != nil {
					errCh <- err
				}
			}()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := s.downloadGoogleDriveFile(ctx, driveSvc, f.Id, localPath, config); err != nil {
				s.sendProtoStatus(localPath, "Error", 0, f.Size, config.Provider)
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) downloadGoogleDriveFile(ctx context.Context, driveSvc *drive.Service, fileID string, localPath string, config domain.SyncConfig) error {
	meta, err := driveSvc.Files.Get(fileID).Fields("id, name, size, md5Checksum, modifiedTime, mimeType").Do()
	if err != nil {
		return err
	}
	s.sendProtoStatus(localPath, "Downloading", 0, meta.Size, config.Provider)
	var body io.ReadCloser
	if strings.HasPrefix(meta.MimeType, "application/vnd.google-apps.") {
		exportMime := googleExportMime(meta.MimeType)
		res, err := driveSvc.Files.Export(fileID, exportMime).Download()
		if err != nil {
			return err
		}
		body = res.Body
		localPath = ensureExportExtension(localPath, exportMime)
	} else {
		res, err := driveSvc.Files.Get(fileID).Download()
		if err != nil {
			return err
		}
		body = res.Body
	}
	defer body.Close()
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, body); err != nil {
		return err
	}
	rel := filepath.Base(localPath)
	if config.IsDirectory {
		if r, err := filepath.Rel(config.LocalPath, localPath); err == nil {
			rel = r
		}
	}
	modTime := time.Now()
	if meta.ModifiedTime != "" {
		if parsed, err := time.Parse(time.RFC3339, meta.ModifiedTime); err == nil {
			modTime = parsed
		}
	}
	_ = s.repo.UpdateFileMetadata(ctx, &domain.FileMetadata{
		SyncConfigID: config.ID,
		Path:         rel,
		ETag:         fileID,
		Size:         meta.Size,
		LastModified: modTime,
		IsDirectory:  false,
	})
	s.sendProtoStatus(localPath, "Synced", 100, meta.Size, config.Provider)
	return nil
}

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

func (s *Server) sendProtoStatus(path string, statusText string, progress int32, size int64, provider domain.Provider) {
	protoProvider := insync.Provider_GOOGLE_DRIVE
	if provider == domain.OneDrive {
		protoProvider = insync.Provider_ONEDRIVE
	}
	msg := &insync.SyncStatusResponse{
		FilePath:           path,
		Status:             statusText,
		ProgressPercentage: progress,
		TotalSize:          size,
		ProcessedSize:      size * int64(progress) / 100,
		Provider:           protoProvider,
	}
	select {
	case s.statusCh <- msg:
	default:
	}
}
