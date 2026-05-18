package grpc

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	authTokenEnv     = "INSYNC_AUTH_TOKEN"
	authTokenFileEnv = "INSYNC_AUTH_TOKEN_FILE"
	allowedRootEnv   = "INSYNC_ALLOWED_ROOT"
)

// EnsureAuthToken returns the local daemon token, creating a 0600 token file when needed.
func EnsureAuthToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv(authTokenEnv)); token != "" {
		return token, nil
	}

	path, err := authTokenPath()
	if err != nil {
		return "", err
	}
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read auth token: %w", err)
	}

	token, err := randomURLToken(32)
	if err != nil {
		return "", fmt.Errorf("generate auth token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create token directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write auth token: %w", err)
	}
	return token, nil
}

func authTokenPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv(authTokenFileEnv)); path != "" {
		return path, nil
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	return filepath.Join(cfgDir, "insync-clone", "auth_token"), nil
}

// NewAuthenticatedGRPCServer creates a gRPC server that requires the local daemon token.
func NewAuthenticatedGRPCServer(token string) *grpc.Server {
	return grpc.NewServer(
		grpc.UnaryInterceptor(authUnaryInterceptor(token)),
		grpc.StreamInterceptor(authStreamInterceptor(token)),
	)
}

// AuthDialOptions returns client interceptors that attach the local daemon token.
func AuthDialOptions(token string) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithUnaryInterceptor(authUnaryClientInterceptor(token)),
		grpc.WithStreamInterceptor(authStreamClientInterceptor(token)),
	}
}

func authUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !authorized(ctx, token) {
			return nil, status.Error(codes.Unauthenticated, "token local inválido")
		}
		return handler(ctx, req)
	}
}

func authStreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !authorized(stream.Context(), token) {
			return status.Error(codes.Unauthenticated, "token local inválido")
		}
		return handler(srv, stream)
	}
}

func authUnaryClientInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req interface{}, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(withAuthToken(ctx, token), method, req, reply, cc, opts...)
	}
}

func authStreamClientInterceptor(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(withAuthToken(ctx, token), desc, cc, method, opts...)
	}
}

func withAuthToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func authorized(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, value := range md.Get("authorization") {
		const prefix = "Bearer "
		if strings.HasPrefix(value, prefix) && constantTimeEqual(strings.TrimPrefix(value, prefix), token) {
			return true
		}
	}
	for _, value := range md.Get("x-insync-auth-token") {
		if constantTimeEqual(value, token) {
			return true
		}
	}
	return false
}

func constantTimeEqual(got string, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func allowedSyncRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv(allowedRootEnv))
	if root == "" {
		return "", nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve allowed sync root: %w", err)
	}
	return filepath.Clean(abs), nil
}

func validatedLocalSyncPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("local_path é obrigatório")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("caminho local inválido: %w", err)
	}
	clean := filepath.Clean(abs)
	root, err := allowedSyncRoot()
	if err != nil {
		return "", err
	}
	if root != "" {
		if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return "", fmt.Errorf("local_path deve ficar dentro de %s", root)
		}
		if err := ensureResolvedPathStaysInRoot(clean, root); err != nil {
			return "", err
		}
	} else if err := ensurePathHasNoUnsafeSymlink(clean); err != nil {
		return "", err
	}
	return clean, nil
}

func safeJoinLocal(base string, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("nome remoto inválido: %q", name)
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) || filepath.Clean(name) != name {
		return "", fmt.Errorf("nome remoto inseguro: %q", name)
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	joined := filepath.Clean(filepath.Join(baseAbs, name))
	if joined != baseAbs && !strings.HasPrefix(joined, baseAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("caminho remoto escapa da raiz local: %q", name)
	}
	return joined, nil
}

func secureMkdirAll(path string, perm fs.FileMode) error {
	if err := ensurePathHasNoUnsafeSymlink(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	return ensurePathHasNoUnsafeSymlink(path)
}

func secureCreateLocalFile(path string) (*os.File, error) {
	if err := ensurePathHasNoUnsafeSymlink(path); err != nil {
		return nil, err
	}
	if err := ensurePathHasNoUnsafeSymlink(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("caminho local inseguro: %s é symlink", path)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
}

func ensureResolvedPathStaysInRoot(path string, root string) error {
	resolvedRoot, rootOK, err := evalExistingPath(root)
	if err != nil {
		return err
	}
	if !rootOK {
		return nil
	}
	resolvedPath, _, err := evalExistingPath(path)
	if err != nil {
		return err
	}
	if resolvedPath != resolvedRoot && !strings.HasPrefix(resolvedPath, resolvedRoot+string(filepath.Separator)) {
		return fmt.Errorf("caminho local inseguro: %s resolve fora de %s", path, root)
	}
	return nil
}

func evalExistingPath(path string) (string, bool, error) {
	clean, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	for {
		resolved, err := filepath.EvalSymlinks(clean)
		if err == nil {
			return filepath.Clean(resolved), true, nil
		}
		if !os.IsNotExist(err) {
			return "", false, err
		}
		parent := filepath.Dir(clean)
		if parent == clean {
			return filepath.Clean(path), false, nil
		}
		clean = parent
	}
}

func ensurePathHasNoUnsafeSymlink(path string) error {
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
