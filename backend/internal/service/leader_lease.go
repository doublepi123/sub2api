package service

import (
	"context"
	"time"
)

// LeaderLease 跨副本领导者租约：让周期性后台任务只在一个副本上运行，
// 避免 N 个副本同时对上游控制面发起请求（本地信号量只限单进程）。
type LeaderLease interface {
	// TryAcquire 尝试获取指定 key 的租约。
	// 其他副本持有时返回 ok=false；返回的 release 必须恰好调用一次安全，
	// 且只会释放调用方自己仍持有的租约。
	TryAcquire(ctx context.Context, key string, ttl time.Duration) (release func(), ok bool, err error)
}

type noopLeaderLease struct{}

// NoopLeaderLease 返回一个总是获取成功的 LeaderLease（release 为空操作），
// 用于无 Redis 的单机部署或测试。
func NoopLeaderLease() LeaderLease {
	return noopLeaderLease{}
}

func (noopLeaderLease) TryAcquire(_ context.Context, _ string, _ time.Duration) (func(), bool, error) {
	return func() {}, true, nil
}
