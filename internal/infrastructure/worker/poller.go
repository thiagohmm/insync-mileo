package worker

import (
	"context"
	"log"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

type RemotePoller struct {
	syncUseCase domain.SyncUseCase
	repo        domain.Repository
	interval    time.Duration
}

func NewRemotePoller(syncUseCase domain.SyncUseCase, repo domain.Repository, interval time.Duration) *RemotePoller {
	return &RemotePoller{
		syncUseCase: syncUseCase,
		repo:        repo,
		interval:    interval,
	}
}

func (p *RemotePoller) Start(ctx context.Context) {
	log.Printf("Remote poller started with interval %v", p.interval)

	// First poll runs immediately on startup.
	p.poll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.poll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (p *RemotePoller) poll(ctx context.Context) {
	configs, err := p.repo.ListSyncConfigs(ctx)
	if err != nil {
		log.Printf("Poller: failed to list sync configs: %v", err)
		return
	}

	for _, config := range configs {
		// Trigger the sync use case which already handles cloud-to-local comparison
		if err := p.syncUseCase.SyncFolder(ctx, config); err != nil {
			log.Printf("Poller: error syncing folder %s: %v", config.LocalPath, err)
		}
	}
}
