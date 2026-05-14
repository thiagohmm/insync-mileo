package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"time"

	"github.com/joho/godotenv"
	"github.com/thiagohmm/insync-clone/api/proto/insync"
	"github.com/thiagohmm/insync-clone/internal/adapters/db"
	"github.com/thiagohmm/insync-clone/internal/adapters/fs"
	"github.com/thiagohmm/insync-clone/internal/domain"
	igrpc "github.com/thiagohmm/insync-clone/internal/infrastructure/grpc"
	"github.com/thiagohmm/insync-clone/internal/infrastructure/worker"
	"github.com/thiagohmm/insync-clone/internal/usecases"
	"google.golang.org/grpc"
)

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	// Initialize Repository
	repo, err := db.NewSQLiteRepository("insync.db")
	if err != nil {
		log.Fatalf("failed to initialize repo: %v", err)
	}
	defer repo.Close()

	// Initialize Use Case
	// Cloud services would be initialized with real clients here
	syncUC := usecases.NewSyncUseCase(repo, []domain.CloudService{})

	// Initialize gRPC Server
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	insync.RegisterInsyncServiceServer(s, igrpc.NewServer(syncUC, repo))

	// Initialize FS Watcher
	watcher, err := fs.NewWatcher(syncUC, repo)
	if err != nil {
		log.Fatalf("failed to initialize watcher: %v", err)
	}
	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watcher.Start(ctx)

	// Initialize Remote Poller (every 5 minutes)
	poller := worker.NewRemotePoller(syncUC, repo, 5*time.Minute)
	go poller.Start(ctx)

	// Start gRPC server
	go func() {
		log.Printf("server listening at %v", lis.Addr())
		if err := s.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	// Wait for interruption
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down...")
	s.GracefulStop()
}
