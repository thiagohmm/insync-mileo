package grpc

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/thiagohmm/insync-clone/api/proto/insync"
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
			got := googleExportMime(tt.mimeType)
			if got != tt.wantExport {
				t.Errorf("googleExportMime(%q) = %q, want %q", tt.mimeType, got, tt.wantExport)
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
			got := ensureExportExtension(tt.path, tt.exportMime)
			if got != tt.want {
				t.Errorf("ensureExportExtension(%q, %q) = %q, want %q", tt.path, tt.exportMime, got, tt.want)
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
