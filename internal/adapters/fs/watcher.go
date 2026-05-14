package fs

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/thiagohmm/insync-clone/internal/domain"
)

type Watcher struct {
	watcher     *fsnotify.Watcher
	syncUseCase domain.SyncUseCase
	repo        domain.Repository
}

func NewWatcher(syncUseCase domain.SyncUseCase, repo domain.Repository) (*Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		watcher:     w,
		syncUseCase: syncUseCase,
		repo:        repo,
	}, nil
}

func (w *Watcher) Start(ctx context.Context) {
	w.addConfiguredPaths(ctx)
	go func() {
		refresh := time.NewTicker(30 * time.Second)
		defer refresh.Stop()
		for {
			select {
			case <-refresh.C:
				w.addConfiguredPaths(ctx)
			case event, ok := <-w.watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
					log.Println("modified file:", event.Name)
					w.syncMatchingConfig(ctx, event.Name)
				}
			case err, ok := <-w.watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (w *Watcher) Add(path string) error {
	return w.watcher.Add(path)
}

func (w *Watcher) addConfiguredPaths(ctx context.Context) {
	configs, err := w.repo.ListSyncConfigs(ctx)
	if err != nil {
		log.Printf("watcher: failed to list configs: %v", err)
		return
	}
	for _, config := range configs {
		path := config.LocalPath
		if !config.IsDirectory {
			path = filepath.Dir(path)
		}
		if _, err := os.Stat(path); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("watcher: stat %s: %v", path, err)
			}
			continue
		}
		if err := w.watcher.Add(path); err != nil {
			log.Printf("watcher: add %s: %v", path, err)
		}
	}
}

func (w *Watcher) syncMatchingConfig(ctx context.Context, changedPath string) {
	configs, err := w.repo.ListSyncConfigs(ctx)
	if err != nil {
		log.Printf("watcher: failed to list configs: %v", err)
		return
	}
	for _, config := range configs {
		if !config.IsDirectory {
			if changedPath != config.LocalPath {
				continue
			}
			if err := w.syncUseCase.SyncFolder(ctx, config); err != nil {
				log.Printf("watcher: sync %s: %v", config.LocalPath, err)
			}
			continue
		}
		if changedPath == config.LocalPath || strings.HasPrefix(changedPath, config.LocalPath+string(filepath.Separator)) {
			if err := w.syncUseCase.SyncFolder(ctx, config); err != nil {
				log.Printf("watcher: sync %s: %v", config.LocalPath, err)
			}
		}
	}
}

func (w *Watcher) Close() error {
	return w.watcher.Close()
}
