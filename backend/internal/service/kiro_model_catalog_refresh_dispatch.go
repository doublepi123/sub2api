package service

import (
	"context"
	"time"
)

func (r *KiroModelCatalogRefresher) runFromRepository(ctx context.Context, repo AccountRepository) {
	r.run(ctx, func(ctx context.Context) ([]*Account, error) {
		listCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		accounts, err := repo.ListByPlatform(listCtx, PlatformKiro)
		if err != nil {
			return nil, err
		}
		pointers := make([]*Account, len(accounts))
		for i := range accounts {
			pointers[i] = &accounts[i]
		}
		return pointers, nil
	})
}

func (s *TokenRefreshService) scheduleKiroModelCatalogRefresh(ctx context.Context) {
	if s.kiroModelCatalogRefresher == nil || ctx.Err() != nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.kiroModelCatalogRefresher.runFromRepository(ctx, s.accountRepo)
	}()
}

func (s *TokenRefreshService) KiroCatalogRefresher() *KiroModelCatalogRefresher {
	return s.kiroModelCatalogRefresher
}
