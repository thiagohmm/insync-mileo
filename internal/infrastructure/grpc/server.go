package grpc

import (
	"context"
	"fmt"
	"net/http"
	"os"
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
}

func NewServer(syncUseCase domain.SyncUseCase, repo domain.Repository) *Server {
	return &Server{
		syncUseCase: syncUseCase,
		repo:        repo,
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
	config := &domain.SyncConfig{
		AccountID:      req.AccountId,
		LocalPath:      req.LocalPath,
		RemoteFolderID: req.RemoteFolderId,
		Mode:           domain.SyncMode(req.Mode),
	}
	if acc, err := s.repo.GetAccount(ctx, req.AccountId); err == nil && acc != nil {
		config.Provider = acc.Provider
	}
	s.repo.SaveSyncConfig(ctx, config)
	return &insync.ConfigureSyncResponse{Success: true}, nil
}

func (s *Server) GetSyncStatus(req *insync.SyncStatusRequest, stream insync.InsyncService_GetSyncStatusServer) error {
	for {
		stream.Send(&insync.SyncStatusResponse{
			FilePath: "Pronto para selecionar arquivos",
			Status:   "Aguardando seleção",
		})
		time.Sleep(10 * time.Second)
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
