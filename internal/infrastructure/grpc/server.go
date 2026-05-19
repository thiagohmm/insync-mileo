package grpc

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	authMu      sync.Mutex
	oauthState  string
	proxyMu     sync.RWMutex
	proxyURL    *url.URL
}

func NewServer(syncUseCase domain.SyncUseCase, repo domain.Repository) *Server {
	s := &Server{
		syncUseCase: syncUseCase,
		repo:        repo,
		statusCh:    make(chan *insync.SyncStatusResponse, 100),
	}
	go s.forwardUseCaseStatuses()
	go s.loadProxyConfig()
	return s
}

func (s *Server) loadProxyConfig() {
	cfg, err := s.repo.GetProxyConfig(context.Background())
	if err != nil || cfg == nil || !cfg.Enabled {
		return
	}
	u, err := url.Parse(fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port))
	if err != nil {
		return
	}
	if cfg.User != "" {
		u.User = url.UserPassword(cfg.User, cfg.Password)
	}
	s.proxyMu.Lock()
	s.proxyURL = u
	s.proxyMu.Unlock()
}

func (s *Server) getProxyURL() *url.URL {
	s.proxyMu.RLock()
	defer s.proxyMu.RUnlock()
	return s.proxyURL
}

// proxyRoundTripper returns an http.RoundTripper that routes through the configured proxy
func (s *Server) proxyRoundTripper() *http.Transport {
	proxy := s.getProxyURL()
	t := &http.Transport{}
	if proxy != nil {
		t.Proxy = http.ProxyURL(proxy)
	}
	return t
}

func (s *Server) forwardUseCaseStatuses() {
	for st := range s.syncUseCase.Statuses() {
		s.sendProtoStatus(st.FilePath, st.Status, st.ProgressPercentage, st.TotalSize)
	}
}

func (s *Server) GetAuthURL(ctx context.Context, req *insync.GetAuthURLRequest) (*insync.GetAuthURLResponse, error) {
	go s.startCallbackServer()

	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	if clientID == "" {
		clientID = "1092767661178-2a973gcsj0cip2oknvdkpsl31vugqrp4.apps.googleusercontent.com"
	}
	state, err := randomURLToken(32)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "gerar state OAuth: %v", err)
	}
	s.authMu.Lock()
	s.oauthState = state
	s.authMu.Unlock()

	values := url.Values{}
	values.Set("client_id", clientID)
	values.Set("redirect_uri", "http://127.0.0.1:8080")
	values.Set("response_type", "code")
	values.Set("scope", "https://www.googleapis.com/auth/drive")
	values.Set("access_type", "offline")
	values.Set("prompt", "consent")
	values.Set("state", state)
	authURL := "https://accounts.google.com/o/oauth2/auth?" + values.Encode()
	return &insync.GetAuthURLResponse{Url: authURL}, nil
}

func (s *Server) startCallbackServer() {
	mux := http.NewServeMux()
	server := &http.Server{
		Addr:              "127.0.0.1:8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		if !s.validOAuthState(state) {
			http.Error(w, "estado OAuth inválido", http.StatusBadRequest)
			return
		}
		if code != "" {
			fmt.Fprintf(w, "<html><body style='font-family:sans-serif;padding-top:50px;text-align:center;'>")
			fmt.Fprintf(w, "<h1 style='color:#4CAF50;'>Autenticação Concluída!</h1>")
			fmt.Fprintf(w, "<p>O Insync Clone já recebeu suas credenciais. Volte para o terminal.</p>")
			fmt.Fprintf(w, "</body></html>")

			// Processa o login imediatamente
			s.AddAccount(context.Background(), &insync.AddAccountRequest{
				AuthCode: code,
			})

			go func() {
				time.Sleep(1 * time.Second)
				server.Shutdown(context.Background())
			}()
		}
	})

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Printf("callback OAuth falhou: %v\n", err)
	}
}

func (s *Server) AddAccount(ctx context.Context, req *insync.AddAccountRequest) (*insync.AddAccountResponse, error) {
	// Código vazio: CLI pergunta se o callback OAuth (navegador → :8080) já gravou a conta.
	if req.AuthCode == "" {
		acc, err := s.repo.GetLatestAccountByProvider(ctx, domain.GoogleDrive)
		if err != nil {
			return &insync.AddAccountResponse{Success: false, ErrorMessage: err.Error()}, nil
		}
		if acc != nil {
			// Valida se o token ainda é válido antes de permitir o uso
			if s.isTokenValid(ctx, acc) {
				return &insync.AddAccountResponse{Success: true, AccountId: acc.ID}, nil
			}
			// Token inválido/expirado, limpa a conta inválida
			fmt.Printf("[INFO] Token inválido para conta %s, removendo...\n", acc.ID)
			s.repo.DeleteAccount(ctx, acc.ID)
			return &insync.AddAccountResponse{Success: false, ErrorMessage: "Token expirado. Autentique novamente no navegador."}, nil
		}
		return &insync.AddAccountResponse{Success: false, ErrorMessage: "Aguardando autorização no navegador..."}, nil
	}

	config := &oauth2.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		Endpoint:     google.Endpoint,
		RedirectURL:  "http://127.0.0.1:8080",
	}

	token, err := config.Exchange(ctx, req.AuthCode)
	if err != nil {
		return &insync.AddAccountResponse{Success: false, ErrorMessage: fmt.Sprintf("falha ao trocar código por token: %v", err)}, nil
	}

	acc := &domain.Account{
		ID:           "google-" + time.Now().Format("150405"),
		Provider:     domain.GoogleDrive,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		Expiry:       domain.NullableTime{Time: token.Expiry, Valid: true},
	}

	s.repo.SaveAccount(ctx, acc)
	fmt.Printf("[DEBUG] AddAccount - Conta salva: ID=%s, HasAccessToken=%v, HasRefreshToken=%v\n", acc.ID, acc.AccessToken != "", acc.RefreshToken != "")
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

	localPath, err := validatedLocalSyncPath(req.LocalPath)
	if err != nil {
		return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: err.Error()}, nil
	}
	isDirectory := req.IsDirectory
	if remote, err := s.getGoogleDriveFile(ctx, acc, req.RemoteFolderId); err == nil {
		isDirectory = remote.MimeType == "application/vnd.google-apps.folder"
	}

	config := &domain.SyncConfig{
		AccountID:      req.AccountId,
		LocalPath:      localPath,
		RemoteFolderID: req.RemoteFolderId,
		Mode:           domain.SyncMode(req.Mode),
		Provider:       acc.Provider,
		IsDirectory:    isDirectory,
	}
	if isDirectory {
		if err := secureMkdirAll(localPath, 0755); err != nil {
			return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: err.Error()}, nil
		}
	} else if err := secureMkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: err.Error()}, nil
	}
	if err := s.repo.SaveSyncConfig(ctx, config); err != nil {
		return &insync.ConfigureSyncResponse{Success: false, ErrorMessage: err.Error()}, nil
	}
	s.sendProtoStatus(req.DisplayName, "Configurado; iniciando sync", 0, 0)
	go s.startGoogleDriveInitialSync(context.Background(), acc, *config)
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

	// Debug: log account info (sem expor tokens sensíveis)
	fmt.Printf("[DEBUG] ListFiles - Account ID: %s, Provider: %s, HasAccessToken: %v, HasRefreshToken: %v\n",
		acc.ID, acc.Provider, acc.AccessToken != "", acc.RefreshToken != "")

	folderID := req.FolderPath
	if folderID == "" {
		folderID = "root"
	}

	files, err := s.listGoogleDriveFolder(ctx, acc, folderID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listar drive: %v", err)
	}
	return &insync.ListFilesResponse{Files: files}, nil
}

func googleOAuthConfig() *oauth2.Config {
	cfg := &oauth2.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		Endpoint:     google.Endpoint,
		RedirectURL:  "http://127.0.0.1:8080",
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "1092767661178-2a973gcsj0cip2oknvdkpsl31vugqrp4.apps.googleusercontent.com"
	}
	// Note: ClientSecret não tem fallback padrão - deve ser definido via variável de ambiente
	return cfg
}

func (s *Server) ConfigureProxy(ctx context.Context, req *insync.ConfigureProxyRequest) (*insync.ConfigureProxyResponse, error) {
	cfg := &domain.ProxyConfig{
		Host:     req.Host,
		Port:     int(req.Port),
		User:     req.User,
		Password: req.Password,
		Enabled:  req.Enabled,
	}
	if err := s.repo.SaveProxyConfig(ctx, cfg); err != nil {
		return &insync.ConfigureProxyResponse{Success: false, ErrorMessage: err.Error()}, nil
	}

	if cfg.Enabled {
		u, err := url.Parse(fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port))
		if err != nil {
			return &insync.ConfigureProxyResponse{Success: false, ErrorMessage: fmt.Sprintf("invalid proxy URL: %v", err)}, nil
		}
		if cfg.User != "" {
			u.User = url.UserPassword(cfg.User, cfg.Password)
		}
		s.proxyMu.Lock()
		s.proxyURL = u
		s.proxyMu.Unlock()
	} else {
		s.proxyMu.Lock()
		s.proxyURL = nil
		s.proxyMu.Unlock()
	}

	return &insync.ConfigureProxyResponse{Success: true}, nil
}

func (s *Server) GetProxyConfig(ctx context.Context, req *insync.GetProxyConfigRequest) (*insync.GetProxyConfigResponse, error) {
	cfg, err := s.repo.GetProxyConfig(ctx)
	if err != nil {
		return &insync.GetProxyConfigResponse{Success: false, ErrorMessage: err.Error()}, nil
	}
	if cfg == nil {
		return &insync.GetProxyConfigResponse{Success: true}, nil
	}
	masked := ""
	if len(cfg.Password) > 0 {
		masked = strings.Repeat("*", len(cfg.Password))
	}
	return &insync.GetProxyConfigResponse{
		Success:        true,
		Host:           cfg.Host,
		Port:           int32(cfg.Port),
		User:           cfg.User,
		PasswordMasked: masked,
		Enabled:        cfg.Enabled,
	}, nil
}

// refreshAndRetry attempts an operation with automatic token refresh on expiration.
// The Go oauth2 package's Client auto-refreshes when the token is near expiry,
// but it can still return "token expired" errors. This function catches that case,
// forces a refresh via the refresh token, persists the new credentials, and retries.
func (s *Server) refreshAndRetry(ctx context.Context, acc *domain.Account, operation func(context.Context, *oauth2.Token) error) error {
	cfg := googleOAuthConfig()
	tok := &oauth2.Token{
		AccessToken:  acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		Expiry:       acc.Expiry.Time,
	}

	// Attempt 1: use the current token (oauth2.Client auto-refreshes if near expiry)
	httpClient := cfg.Client(ctx, tok)
	if pt := s.proxyRoundTripper(); pt != nil {
		httpClient.Transport = pt
	}
	ctxWithHTTPClient := context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	if opErr := operation(ctxWithHTTPClient, tok); opErr != nil {
		// First attempt failed — force a token refresh via refresh_token
		if acc.RefreshToken == "" {
			return fmt.Errorf("token expired and no refresh token available (first error: %w)", opErr)
		}

		tokenSource := cfg.TokenSource(ctx, tok)
		newTok, tokErr := tokenSource.Token()
		if tokErr != nil {
			return fmt.Errorf("token refresh failed: %w (first attempt error: %w)", tokErr, opErr)
		}

		// Persist refreshed credentials back to the database
		acc.AccessToken = newTok.AccessToken
		if newTok.RefreshToken != "" {
			acc.RefreshToken = newTok.RefreshToken
		}
		acc.Expiry = domain.NullableTime{Time: newTok.Expiry, Valid: true}
		if saveErr := s.repo.SaveAccount(ctx, acc); saveErr != nil {
			fmt.Printf("failed to save refreshed token: %v\n", saveErr)
		}

		// Attempt 2: retry with the refreshed token
		refreshedHTTPClient := cfg.Client(ctx, newTok)
		if pt := s.proxyRoundTripper(); pt != nil {
			refreshedHTTPClient.Transport = pt
		}
		refreshedCtx := context.WithValue(ctx, oauth2.HTTPClient, refreshedHTTPClient)
		return operation(refreshedCtx, newTok)
	}

	return nil
}

// isTokenValid verifica se o token de acesso ainda é válido fazendo uma requisição simples ao Drive
func (s *Server) isTokenValid(ctx context.Context, acc *domain.Account) bool {
	cfg := googleOAuthConfig()
	tok := &oauth2.Token{
		AccessToken:  acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		Expiry:       acc.Expiry.Time,
	}

	httpClient := cfg.Client(ctx, tok)
	if pt := s.proxyRoundTripper(); pt != nil {
		httpClient.Transport = pt
	}
	driveSvc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		fmt.Printf("[DEBUG] isTokenValid - Erro ao criar Drive service: %v\n", err)
		return false
	}

	// Tenta fazer uma requisição simples: obter informações do usuário (about)
	_, err = driveSvc.About.Get().Fields("user/emailAddress").Context(ctx).Do()
	if err != nil {
		fmt.Printf("[DEBUG] isTokenValid - Token inválido: %v\n", err)
		return false
	}

	fmt.Printf("[DEBUG] isTokenValid - Token válido para conta %s\n", acc.ID)
	return true
}

func (s *Server) listGoogleDriveFolder(ctx context.Context, acc *domain.Account, folderID string) ([]*insync.FileInfo, error) {
	var files []*insync.FileInfo
	err := s.refreshAndRetry(ctx, acc, func(ctx context.Context, tok *oauth2.Token) error {
		if tok == nil || tok.AccessToken == "" {
			return fmt.Errorf("token de autenticação inválido ou ausente")
		}
		cfg := googleOAuthConfig()
		httpClient := cfg.Client(ctx, tok)
		if pt := s.proxyRoundTripper(); pt != nil {
			httpClient.Transport = pt
		}
		driveSvc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
		if err != nil {
			return err
		}
		svc := cloud.NewGoogleDriveService(driveSvc)
		domainFiles, err := svc.ListFiles(ctx, folderID)
		if err != nil {
			return err
		}
		for _, f := range domainFiles {
			lastMod := ""
			if !f.LastModified.IsZero() {
				lastMod = f.LastModified.UTC().Format(time.RFC3339)
			}
			out := &insync.FileInfo{
				Name:         f.Path,
				Path:         f.ETag,
				IsDirectory:  f.IsDirectory,
				Size:         f.Size,
				Etag:         f.ETag,
				LastModified: lastMod,
			}
			files = append(files, out)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
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

func (s *Server) Unsync(ctx context.Context, req *insync.UnsyncRequest) (*insync.UnsyncResponse, error) {
	if req.AccountId == "" || req.RemoteFolderId == "" {
		return &insync.UnsyncResponse{Success: false, ErrorMessage: "account_id e remote_folder_id são obrigatórios"}, nil
	}

	// Remover o sync config do banco de dados
	err := s.repo.DeleteSyncConfigByRemoteID(ctx, req.AccountId, req.RemoteFolderId)
	if err != nil {
		return &insync.UnsyncResponse{Success: false, ErrorMessage: fmt.Sprintf("failed to delete sync config: %v", err)}, nil
	}

	// Enviar status de sucesso
	s.sendProtoStatus(req.RemoteFolderId, "Unsynced - removed from local", 100, 0)

	return &insync.UnsyncResponse{Success: true}, nil
}

func (s *Server) getGoogleDriveFile(ctx context.Context, acc *domain.Account, fileID string) (*drive.File, error) {
	var file *drive.File
	err := s.refreshAndRetry(ctx, acc, func(ctx context.Context, tok *oauth2.Token) error {
		cfg := googleOAuthConfig()
		httpClient := cfg.Client(ctx, tok)
		if pt := s.proxyRoundTripper(); pt != nil {
			httpClient.Transport = pt
		}
		driveSvc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
		if err != nil {
			return err
		}
		var errGet error
		file, errGet = driveSvc.Files.Get(fileID).Fields("id, name, size, md5Checksum, modifiedTime, mimeType").Do()
		return errGet
	})
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (s *Server) googleDriveService(ctx context.Context, acc *domain.Account) (*drive.Service, error) {
	cfg := googleOAuthConfig()
	tok := &oauth2.Token{
		AccessToken:  acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		Expiry:       acc.Expiry.Time,
	}

	httpClient := cfg.Client(ctx, tok)
	if pt := s.proxyRoundTripper(); pt != nil {
		httpClient.Transport = pt
	}
	return drive.NewService(ctx, option.WithHTTPClient(httpClient))
}

func (s *Server) startGoogleDriveInitialSync(ctx context.Context, acc *domain.Account, config domain.SyncConfig) {
	var driveSvc *drive.Service
	var err error

	err = s.refreshAndRetry(ctx, acc, func(ctx context.Context, tok *oauth2.Token) error {
		cfg := googleOAuthConfig()
		httpClient := cfg.Client(ctx, tok)
		if pt := s.proxyRoundTripper(); pt != nil {
			httpClient.Transport = pt
		}
		driveSvc, err = drive.NewService(ctx, option.WithHTTPClient(httpClient))
		return err
	})

	if err != nil {
		s.sendProtoStatus(config.LocalPath, "Error", 0, 0)
		return
	}
	if config.IsDirectory {
		if err := secureMkdirAll(config.LocalPath, 0755); err != nil {
			s.sendProtoStatus(config.LocalPath, "Error", 0, 0)
			return
		}
		if err := s.downloadGoogleDriveFolder(ctx, driveSvc, config.RemoteFolderID, config.LocalPath, config); err != nil {
			s.sendProtoStatus(config.LocalPath, "Error", 0, 0)
		}
		return
	}
	if err := secureMkdirAll(filepath.Dir(config.LocalPath), 0755); err != nil {
		s.sendProtoStatus(config.LocalPath, "Error", 0, 0)
		return
	}
	if err := s.downloadGoogleDriveFile(ctx, driveSvc, config.RemoteFolderID, config.LocalPath, config); err != nil {
		s.sendProtoStatus(config.LocalPath, "Error", 0, 0)
	}
}

func (s *Server) downloadGoogleDriveFolder(ctx context.Context, driveSvc *drive.Service, folderID string, localDir string, config domain.SyncConfig) error {
	res, err := driveSvc.Files.List().
		Q(driveParentQuery(folderID)).
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
		localPath, err := safeJoinLocal(localDir, f.Name)
		if err != nil {
			return err
		}
		if f.MimeType == "application/vnd.google-apps.folder" {
			if err := secureMkdirAll(localPath, 0755); err != nil {
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
				s.sendProtoStatus(localPath, "Error", 0, f.Size)
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
	s.sendProtoStatus(localPath, "Downloading", 0, meta.Size)

	var body io.ReadCloser
	if strings.HasPrefix(meta.MimeType, "application/vnd.google-apps.") {
		exportMime := cloud.GoogleExportMime(meta.MimeType)
		res, err := driveSvc.Files.Export(fileID, exportMime).Download()
		if err != nil {
			return err
		}
		body = res.Body
		localPath = cloud.EnsureExportExtension(localPath, exportMime)
	} else {
		res, err := driveSvc.Files.Get(fileID).Download()
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

	// Track download progress
	totalSize := meta.Size

	wrapped := &progressReader{
		reader: body,
		total:  totalSize,
		onProgress: func(d, _ int64) {
			if totalSize > 0 {
				progress := int32(float64(d) / float64(totalSize) * 100)
				s.sendProtoStatus(localPath, "Downloading", progress, totalSize)
			}
		},
	}

	if _, err := io.Copy(out, wrapped); err != nil {
		return err
	}

	// Verify download integrity via MD5 checksum when available.
	if meta.Md5Checksum != "" {
		if cerr := s.verifyLocalMD5(localPath, meta.Md5Checksum); cerr != nil {
			fmt.Printf("checksum mismatch for %s: %v\n", localPath, cerr)
		}
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
		MD5Checksum:  meta.Md5Checksum,
	})
	s.sendProtoStatus(localPath, "Synced", 100, meta.Size)
	return nil
}

// verifyLocalMD5 computes the MD5 hash of the local file and compares it
// against expectedHex. Returns an error on mismatch.
func (s *Server) verifyLocalMD5(localPath, expectedHex string) error {
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

func (s *Server) validOAuthState(state string) bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if state == "" || s.oauthState == "" || state != s.oauthState {
		return false
	}
	s.oauthState = ""
	return true
}

func randomURLToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func driveParentQuery(folderID string) string {
	return fmt.Sprintf("'%s' in parents and trashed = false", escapeDriveQueryString(folderID))
}

func escapeDriveQueryString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `'`, `\'`)
	return value
}

func (s *Server) sendProtoStatus(path string, statusText string, progress int32, size int64) {
	msg := &insync.SyncStatusResponse{
		FilePath:           path,
		Status:             statusText,
		ProgressPercentage: progress,
		TotalSize:          size,
		ProcessedSize:      size * int64(progress) / 100,
	}
	select {
	case s.statusCh <- msg:
	default:
	}
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
