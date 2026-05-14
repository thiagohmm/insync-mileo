package fs

import (
	"context"
	"log"

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
	go func() {
		for {
			select {
			case event, ok := <-w.watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					log.Println("modified file:", event.Name)
					// Trigger sync for the folder containing this file
					// In a real app, we'd find the SyncConfig for this path
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

func (w *Watcher) Close() error {
	return w.watcher.Close()
}
