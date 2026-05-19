package grpc

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thiagohmm/insync-clone/api/proto/insync"
	"github.com/thiagohmm/insync-clone/internal/adapters/cloud"
	"github.com/thiagohmm/insync-clone/internal/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestGoogleExportMime(t *testing.T) {
	tests := []struct {
		name       string
		mimeType   string
		wantExport string
	}{
		{
			name:       "spreadsheet exports to xlsx",
			mimeType:   "application/vnd.google-apps.spreadsheet",
			wantExport: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		},
		{
			name:       "presentation exports to pptx",
			mimeType:   "application/vnd.google-apps.presentation",
			wantExport: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		},
		{
			name:       "document exports to pdf",
			mimeType:   "application/vnd.google-apps.document",
			wantExport: "application/pdf",
		},
		{
			name:       "drawing exports to pdf",
			mimeType:   "application/vnd.google-apps.drawing",
			wantExport: "application/pdf",
		},
		{
			name:       "unknown google mime exports to pdf",
			mimeType:   "application/vnd.google-apps.unknown",
			wantExport: "application/pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cloud.GoogleExportMime(tt.mimeType)
			if got != tt.wantExport {
				t.Errorf("GoogleExportMime(%q) = %q, want %q", tt.mimeType, got, tt.wantExport)
			}
		})
	}
}

func TestValidatedLocalSyncPathRequiresAllowedRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv(allowedRootEnv, root)

	got, err := validatedLocalSyncPath(filepath.Join(root, "folder"))
	if err != nil {
		t.Fatalf("validatedLocalSyncPath() error: %v", err)
	}
	if got != filepath.Join(root, "folder") {
		t.Fatalf("validatedLocalSyncPath() = %q, want path inside root", got)
	}

	_, err = validatedLocalSyncPath(filepath.Join(t.TempDir(), "outside"))
	if err == nil {
		t.Fatal("expected path outside allowed root to be rejected")
	}
}

func TestValidatedLocalSyncPathAllowsAnyPathWhenAllowedRootUnset(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error: %v", err)
	}
	path := filepath.Join(wd, "custom-sync")

	got, err := validatedLocalSyncPath(path)
	if err != nil {
		t.Fatalf("validatedLocalSyncPath() error: %v", err)
	}
	if got != path {
		t.Fatalf("validatedLocalSyncPath() = %q, want %q", got, path)
	}
}

func TestValidatedLocalSyncPathRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	t.Setenv(allowedRootEnv, root)

	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := validatedLocalSyncPath(filepath.Join(link, "file.txt"))
	if err == nil {
		t.Fatal("expected symlink path to be rejected")
	}
}

func TestSafeJoinLocalRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	tests := []string{"../secret.txt", "nested/file.txt", "/tmp/secret.txt", "..", "."}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := safeJoinLocal(base, name); err == nil {
				t.Fatalf("expected %q to be rejected", name)
			}
		})
	}

	got, err := safeJoinLocal(base, "file.txt")
	if err != nil {
		t.Fatalf("safeJoinLocal() error: %v", err)
	}
	if got != filepath.Join(base, "file.txt") {
		t.Fatalf("safeJoinLocal() = %q, want %q", got, filepath.Join(base, "file.txt"))
	}
}

func TestSecureCreateLocalFileRejectsSymlinkDestination(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	f, err := secureCreateLocalFile(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("expected symlink destination to be rejected")
	}
}

func TestConfigureSyncRejectsPathOutsideAllowedRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv(allowedRootEnv, root)

	repo := domain.NewMockRepository()
	repo.Accounts["acct-1"] = &domain.Account{ID: "acct-1", Provider: domain.GoogleDrive}
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.ConfigureSync(context.Background(), &insync.ConfigureSyncRequest{
		AccountId:      "acct-1",
		LocalPath:      filepath.Join(t.TempDir(), "outside"),
		RemoteFolderId: "remote-1",
		IsDirectory:    true,
	})
	if err != nil {
		t.Fatalf("ConfigureSync() transport error: %v", err)
	}
	if resp.GetSuccess() {
		t.Fatal("expected ConfigureSync to reject path outside allowed root")
	}
	if repo.SavedSyncConfig != nil {
		t.Fatal("expected rejected sync config not to be saved")
	}
}

func TestDriveParentQueryEscapesFolderID(t *testing.T) {
	got := driveParentQuery(`abc'\def`)
	want := `'abc\'\\def' in parents and trashed = false`
	if got != want {
		t.Fatalf("driveParentQuery() = %q, want %q", got, want)
	}
}

func TestOAuthStateIsSingleUse(t *testing.T) {
	srv := NewServer(domain.NewMockSyncUseCase(), domain.NewMockRepository())
	srv.oauthState = "state-1"

	if !srv.validOAuthState("state-1") {
		t.Fatal("expected matching state to be valid")
	}
	if srv.validOAuthState("state-1") {
		t.Fatal("expected OAuth state to be single-use")
	}
	if srv.validOAuthState("other") {
		t.Fatal("expected different state to be rejected")
	}
}

func TestAuthUnaryInterceptorRejectsMissingToken(t *testing.T) {
	interceptor := authUnaryInterceptor("secret-token")
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("status.Code() = %v, want %v", status.Code(err), codes.Unauthenticated)
	}
}

func TestAuthUnaryInterceptorAcceptsBearerToken(t *testing.T) {
	interceptor := authUnaryInterceptor("secret-token")
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret-token"))

	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("interceptor response = %v, want ok", resp)
	}
}

func TestEnsureAuthTokenCreatesPrivateFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "auth_token")
	t.Setenv(authTokenEnv, "")
	t.Setenv(authTokenFileEnv, tokenFile)

	token, err := EnsureAuthToken()
	if err != nil {
		t.Fatalf("EnsureAuthToken() error: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("token file permissions = %v, want 0600", got)
	}
}

func TestEnsureExportExtension(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		exportMime string
		want       string
	}{
		{
			name:       "no extension - spreadsheet gets xlsx",
			path:       "/tmp/Sheet1",
			exportMime: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			want:       "/tmp/Sheet1.xlsx",
		},
		{
			name:       "no extension - presentation gets pptx",
			path:       "/tmp/Slides",
			exportMime: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
			want:       "/tmp/Slides.pptx",
		},
		{
			name:       "no extension - default gets pdf",
			path:       "/tmp/Doc1",
			exportMime: "application/pdf",
			want:       "/tmp/Doc1.pdf",
		},
		{
			name:       "already has extension - unchanged",
			path:       "/tmp/Doc1.docx",
			exportMime: "application/pdf",
			want:       "/tmp/Doc1.docx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cloud.EnsureExportExtension(tt.path, tt.exportMime)
			if got != tt.want {
				t.Errorf("EnsureExportExtension(%q, %q) = %q, want %q", tt.path, tt.exportMime, got, tt.want)
			}
		})
	}
}

// -----------------------------
// MD5 checksum verification tests
// -----------------------------

func TestVerifyLocalMD5(t *testing.T) {
	srv := NewServer(domain.NewMockSyncUseCase(), domain.NewMockRepository())
	tmpDir := t.TempDir()

	t.Run("match", func(t *testing.T) {
		path := filepath.Join(tmpDir, "check.txt")
		const content = "checksum test data"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		expected := computeMD5ForTest(content)
		if err := srv.verifyLocalMD5(path, expected); err != nil {
			t.Errorf("expected match, got error: %v", err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		path := filepath.Join(tmpDir, "bad.txt")
		if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
		err := srv.verifyLocalMD5(path, "00000000000000000000000000000000")
		if err == nil {
			t.Error("expected mismatch error")
		}
	})

	t.Run("file not found", func(t *testing.T) {
		err := srv.verifyLocalMD5(filepath.Join(tmpDir, "missing.txt"), "00000000000000000000000000000000")
		if err == nil {
			t.Error("expected error for missing file")
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		path := filepath.Join(tmpDir, "case.txt")
		if err := os.WriteFile(path, []byte("case"), 0644); err != nil {
			t.Fatal(err)
		}
		expected := computeMD5ForTest("case")
		if err := srv.verifyLocalMD5(path, expected); err != nil {
			t.Errorf("expected case-insensitive match, got error: %v", err)
		}
	})

	t.Run("non-empty file vs empty expected hex", func(t *testing.T) {
		// When expectedHex is empty but the file has content, it should be a mismatch.
		path := filepath.Join(tmpDir, "skip.txt")
		if err := os.WriteFile(path, []byte("skip"), 0644); err != nil {
			t.Fatal(err)
		}
		err := srv.verifyLocalMD5(path, "")
		if err == nil {
			t.Error("expected mismatch when expectedHex is empty but file has content")
		}
	})
}

func computeMD5ForTest(data string) string {
	h := md5.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// -----------------------------
// Proxy configuration tests
// -----------------------------

func TestConfigureProxy_EnableWithHostAndPort(t *testing.T) {
	repo := domain.NewMockRepository()
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
		Host:    "proxy.example.com",
		Port:    8080,
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("ConfigureProxy() transport error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success, got error: %s", resp.GetErrorMessage())
	}

	// Verify saved in repo
	saved, err := repo.GetProxyConfig(context.Background())
	if err != nil {
		t.Fatalf("GetProxyConfig() error: %v", err)
	}
	if saved == nil {
		t.Fatal("expected proxy config in repo, got nil")
	}
	if saved.Host != "proxy.example.com" {
		t.Errorf("Host = %s, want proxy.example.com", saved.Host)
	}
	if saved.Port != 8080 {
		t.Errorf("Port = %d, want 8080", saved.Port)
	}

	// Verify proxy URL is set on server
	proxyURL := srv.getProxyURL()
	if proxyURL == nil {
		t.Fatal("expected proxy URL to be set on server, got nil")
	}
	if proxyURL.Host != "proxy.example.com:8080" {
		t.Errorf("proxyURL.Host = %s, want proxy.example.com:8080", proxyURL.Host)
	}
}

func TestConfigureProxy_EnableWithAuth(t *testing.T) {
	repo := domain.NewMockRepository()
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
		Host:     "proxy.example.com",
		Port:     3128,
		User:     "proxyuser",
		Password: "proxypass",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("ConfigureProxy() transport error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success, got error: %s", resp.GetErrorMessage())
	}

	proxyURL := srv.getProxyURL()
	if proxyURL == nil {
		t.Fatal("expected proxy URL to be set, got nil")
	}
	user := proxyURL.User.Username()
	pass, hasPass := proxyURL.User.Password()
	if user != "proxyuser" {
		t.Errorf("user = %s, want proxyuser", user)
	}
	if !hasPass || pass != "proxypass" {
		t.Errorf("password mismatch, got hasPass=%v pass=%s", hasPass, pass)
	}
}

func TestConfigureProxy_Disable(t *testing.T) {
	repo := domain.NewMockRepository()
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	// First enable
	_, _ = srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
		Host:    "proxy.example.com",
		Port:    8080,
		Enabled: true,
	})

	if srv.getProxyURL() == nil {
		t.Fatal("proxy should be enabled at this point")
	}

	// Now disable
	resp, err := srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
		Host:    "proxy.example.com",
		Port:    8080,
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("ConfigureProxy() transport error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success, got error: %s", resp.GetErrorMessage())
	}

	if srv.getProxyURL() != nil {
		t.Error("expected proxy URL to be nil after disabling")
	}
}

func TestConfigureProxy_RepoError(t *testing.T) {
	repo := domain.NewMockRepository()
	repo.ErrorOn["SaveProxyConfig"] = fmt.Errorf("db error")
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
		Host:    "proxy.example.com",
		Port:    8080,
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("ConfigureProxy() transport error: %v", err)
	}
	if resp.GetSuccess() {
		t.Error("expected failure when repo returns error")
	}
	if resp.GetErrorMessage() == "" {
		t.Error("expected error message")
	}
}

func TestGetProxyConfig_NoConfig(t *testing.T) {
	repo := domain.NewMockRepository()
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.GetProxyConfig(context.Background(), &insync.GetProxyConfigRequest{})
	if err != nil {
		t.Fatalf("GetProxyConfig() transport error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success, got error: %s", resp.GetErrorMessage())
	}
	if resp.GetHost() != "" {
		t.Errorf("expected empty host, got %s", resp.GetHost())
	}
}

func TestGetProxyConfig_WithConfig(t *testing.T) {
	repo := domain.NewMockRepository()
	repo.SaveProxyConfig(context.Background(), &domain.ProxyConfig{
		Host:     "proxy.example.com",
		Port:     8080,
		User:     "proxyuser",
		Password: "secret123",
		Enabled:  true,
	})
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.GetProxyConfig(context.Background(), &insync.GetProxyConfigRequest{})
	if err != nil {
		t.Fatalf("GetProxyConfig() transport error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success, got error: %s", resp.GetErrorMessage())
	}
	if resp.GetHost() != "proxy.example.com" {
		t.Errorf("Host = %s, want proxy.example.com", resp.GetHost())
	}
	if resp.GetPort() != 8080 {
		t.Errorf("Port = %d, want 8080", resp.GetPort())
	}
	if resp.GetUser() != "proxyuser" {
		t.Errorf("User = %s, want proxyuser", resp.GetUser())
	}
	if !resp.GetEnabled() {
		t.Error("Expected Enabled = true")
	}
}

func TestGetProxyConfig_PasswordMasked(t *testing.T) {
	repo := domain.NewMockRepository()
	repo.SaveProxyConfig(context.Background(), &domain.ProxyConfig{
		Host:     "proxy.example.com",
		Port:     8080,
		Password: "secret123",
		Enabled:  true,
	})
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.GetProxyConfig(context.Background(), &insync.GetProxyConfigRequest{})
	if err != nil {
		t.Fatalf("GetProxyConfig() transport error: %v", err)
	}
	masked := resp.GetPasswordMasked()
	if masked == "secret123" {
		t.Error("password should be masked, not returned in plaintext")
	}
	if masked != "*********" {
		t.Errorf("PasswordMasked = %s, want *********", masked)
	}
}

func TestGetProxyConfig_EmptyPassword(t *testing.T) {
	repo := domain.NewMockRepository()
	repo.SaveProxyConfig(context.Background(), &domain.ProxyConfig{
		Host:    "proxy.example.com",
		Port:    8080,
		Enabled: true,
	})
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.GetProxyConfig(context.Background(), &insync.GetProxyConfigRequest{})
	if err != nil {
		t.Fatalf("GetProxyConfig() transport error: %v", err)
	}
	masked := resp.GetPasswordMasked()
	if masked != "" {
		t.Errorf("PasswordMasked = %s, want empty", masked)
	}
}

func TestGetProxyConfig_RepoError(t *testing.T) {
	repo := domain.NewMockRepository()
	repo.ErrorOn["GetProxyConfig"] = fmt.Errorf("db error")
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	resp, err := srv.GetProxyConfig(context.Background(), &insync.GetProxyConfigRequest{})
	if err != nil {
		t.Fatalf("GetProxyConfig() transport error: %v", err)
	}
	if resp.GetSuccess() {
		t.Error("expected failure when repo returns error")
	}
}

func TestProxyRoundTripper_NoProxy(t *testing.T) {
	repo := domain.NewMockRepository()
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	transport := srv.proxyRoundTripper()
	if transport == nil {
		t.Fatal("expected non-nil transport")
	}

	// When no proxy configured, proxyURL should be nil on the server
	if srv.getProxyURL() != nil {
		t.Error("expected proxy URL to be nil when no proxy configured")
	}
}

func TestProxyRoundTripper_WithProxy(t *testing.T) {
	repo := domain.NewMockRepository()
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	_, _ = srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
		Host:    "proxy.example.com",
		Port:    8080,
		Enabled: true,
	})

	transport := srv.proxyRoundTripper()
	if transport == nil {
		t.Fatal("expected non-nil transport")
	}
	if transport.Proxy == nil {
		t.Fatal("expected Proxy to be set on transport")
	}

	proxyURL, err := transport.Proxy(&http.Request{Header: http.Header{}})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL.Host != "proxy.example.com:8080" {
		t.Errorf("proxyURL.Host = %s, want proxy.example.com:8080", proxyURL.Host)
	}
}

func TestProxyRoundTripper_AfterDisable(t *testing.T) {
	repo := domain.NewMockRepository()
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	_, _ = srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
		Host:    "proxy.example.com",
		Port:    8080,
		Enabled: true,
	})

	transport := srv.proxyRoundTripper()
	proxyURL, _ := transport.Proxy(&http.Request{Header: http.Header{}})
	if proxyURL.Host != "proxy.example.com:8080" {
		t.Fatal("proxy should be enabled at this point")
	}

	_, _ = srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
		Enabled: false,
	})

	transport2 := srv.proxyRoundTripper()
	if transport2.Proxy != nil {
		t.Error("expected Proxy to be nil after disabling proxy")
	}
}

func TestProxyURL_ThreadSafe(t *testing.T) {
	repo := domain.NewMockRepository()
	srv := NewServer(domain.NewMockSyncUseCase(), repo)

	var wg sync.WaitGroup

	// Concurrent writes
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			srv.ConfigureProxy(context.Background(), &insync.ConfigureProxyRequest{
				Host:    fmt.Sprintf("proxy-%d.com", i),
				Port:    int32(8000 + i),
				Enabled: true,
			})
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.getProxyURL()
			srv.proxyRoundTripper()
		}()
	}

	wg.Wait()

	// Final state should be consistent
	proxyURL := srv.getProxyURL()
	if proxyURL == nil {
		t.Fatal("expected proxy URL to be set, got nil")
	}
}
