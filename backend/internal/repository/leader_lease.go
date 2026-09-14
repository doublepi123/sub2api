package repository

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// leaderLeaseReleaseScript 仅当 key 当前持有者仍为本实例时才删除（compare-and-delete），
// 防止本实例租约已过期并被其他副本重新获取后，被本实例误删。
var leaderLeaseReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

var leaderLeaseNoRedisWarnOnce sync.Once

type redisLeaderLease struct {
	rdb        *redis.Client
	instanceID string
}

// NewRedisLeaderLease 创建基于 Redis SetNX 的跨副本领导者租约。
// rdb 为 nil 时退化为 service.NoopLeaderLease()（单机部署），并仅告警一次。
func NewRedisLeaderLease(rdb *redis.Client, instanceID string) service.LeaderLease {
	if rdb == nil {
		leaderLeaseNoRedisWarnOnce.Do(func() {
			slog.Warn("cluster lease unavailable; running locally")
		})
		return service.NoopLeaderLease()
	}
	return &redisLeaderLease{rdb: rdb, instanceID: instanceID}
}

func (l *redisLeaderLease) TryAcquire(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	ok, err := l.rdb.SetNX(ctx, key, l.instanceID, ttl).Result()
	if err != nil {
		// 上抛错误，由调用方决定（调用方应 fail-closed，避免 Redis 抖动时多副本同时跑）
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = leaderLeaseReleaseScript.Run(releaseCtx, l.rdb, []string{key}, l.instanceID).Result()
		})
	}
	return release, true, nil
}

// ProvideLeaderLease 为 Wire 提供 LeaderLease：实例 ID 在进程启动时生成一次。
func ProvideLeaderLease(rdb *redis.Client) service.LeaderLease {
	return NewRedisLeaderLease(rdb, uuid.NewString())
}
