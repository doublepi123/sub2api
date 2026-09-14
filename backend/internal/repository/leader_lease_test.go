package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newLeaderLeaseRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func TestRedisLeaderLease_NilClientFallsBackToNoop(t *testing.T) {
	// Given a nil Redis client (single-node deployment without Redis)
	lease := NewRedisLeaderLease(nil, "instance-a")

	// When acquiring a lease
	release, ok, err := lease.TryAcquire(context.Background(), "leader:probe", time.Minute)

	// Then it falls back to the noop lease: always acquires, no panic, no error
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, release)
	require.NotPanics(t, release)
}

func TestRedisLeaderLease_SecondAcquireFailsWhileHeld(t *testing.T) {
	// Given replica A holding the lease
	_, rdb := newLeaderLeaseRedis(t)
	ctx := context.Background()
	leaseA := NewRedisLeaderLease(rdb, "instance-a")
	leaseB := NewRedisLeaderLease(rdb, "instance-b")

	releaseA, ok, err := leaseA.TryAcquire(ctx, "leader:probe", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, releaseA)

	// When replica B tries to acquire the same key while A holds it
	releaseB, ok, err := leaseB.TryAcquire(ctx, "leader:probe", time.Minute)

	// Then B is denied without error and gets no release func
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, releaseB)
}

func TestRedisLeaderLease_ReleaseOnlyDeletesOwnLease(t *testing.T) {
	// Given replica A acquired the lease but let it expire, and replica B re-acquired it
	mr, rdb := newLeaderLeaseRedis(t)
	ctx := context.Background()
	const key = "leader:probe"
	leaseA := NewRedisLeaderLease(rdb, "instance-a")
	leaseB := NewRedisLeaderLease(rdb, "instance-b")

	releaseA, ok, err := leaseA.TryAcquire(ctx, key, 50*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)

	// Force-expire A's lease so B can take over (simulates A stalling past TTL)
	mr.FastForward(time.Second)

	releaseB, ok, err := leaseB.TryAcquire(ctx, key, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, releaseB)

	// When the stalled A finally calls its (late) release
	releaseA()

	// Then B's lease must survive: the key still holds B's instance id
	owner, err := mr.Get(key)
	require.NoError(t, err)
	require.Equal(t, "instance-b", owner)

	// And only B's own release deletes the key
	releaseB()
	require.False(t, mr.Exists(key))
}

func TestRedisLeaderLease_ReacquireAfterRelease(t *testing.T) {
	// Given a lease that was acquired and released by replica A
	mr, rdb := newLeaderLeaseRedis(t)
	ctx := context.Background()
	const key = "leader:probe"
	leaseA := NewRedisLeaderLease(rdb, "instance-a")
	leaseB := NewRedisLeaderLease(rdb, "instance-b")

	releaseA, ok, err := leaseA.TryAcquire(ctx, key, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	releaseA()
	require.False(t, mr.Exists(key))

	// When another replica acquires after the release
	releaseB, ok, err := leaseB.TryAcquire(ctx, key, time.Minute)

	// Then it succeeds
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, releaseB)
	owner, err := mr.Get(key)
	require.NoError(t, err)
	require.Equal(t, "instance-b", owner)
}

func TestRedisLeaderLease_RedisErrorIsReturned(t *testing.T) {
	// Given a lease whose Redis server has gone away
	mr, rdb := newLeaderLeaseRedis(t)
	lease := NewRedisLeaderLease(rdb, "instance-a")
	mr.Close()

	// When acquiring fails at the Redis layer
	release, ok, err := lease.TryAcquire(context.Background(), "leader:probe", time.Minute)

	// Then the error is surfaced so the caller can fail closed
	require.Error(t, err)
	require.False(t, ok)
	require.Nil(t, release)
}

var _ service.LeaderLease = NewRedisLeaderLease(nil, "compile-check")
var _ service.RenewableLeaderLease = (*redisLeaderLease)(nil)

func TestRedisLeaderLease_RenewExtendsOnlyOwnLease(t *testing.T) {
	// Given replica A holding a short lease, and a separate key owned by replica B
	mr, rdb := newLeaderLeaseRedis(t)
	ctx := context.Background()
	leaseA := NewRedisLeaderLease(rdb, "instance-a").(service.RenewableLeaderLease)
	leaseB := NewRedisLeaderLease(rdb, "instance-b").(service.RenewableLeaderLease)

	_, ok, err := leaseA.TryAcquire(ctx, "leader:a", 100*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = leaseB.TryAcquire(ctx, "leader:b", 100*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)

	// When A renews its own lease with a longer TTL
	ok, err = leaseA.Renew(ctx, "leader:a", time.Hour)

	// Then renewal succeeds and the key's TTL actually grew
	require.NoError(t, err)
	require.True(t, ok)
	require.Greater(t, mr.TTL("leader:a"), 100*time.Millisecond)

	// When A tries to renew B's key
	ttlBefore := mr.TTL("leader:b")
	ok, err = leaseA.Renew(ctx, "leader:b", time.Hour)

	// Then it is denied without error and B's TTL is unchanged
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, ttlBefore, mr.TTL("leader:b"))
}

func TestRedisLeaderLease_RenewAfterExpiryReturnsFalse(t *testing.T) {
	// Given a lease that has already expired (ownership lost)
	mr, rdb := newLeaderLeaseRedis(t)
	ctx := context.Background()
	lease := NewRedisLeaderLease(rdb, "instance-a").(service.RenewableLeaderLease)

	_, ok, err := lease.TryAcquire(ctx, "leader:probe", 50*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)
	mr.FastForward(time.Second)

	// When renewing after expiry
	ok, err = lease.Renew(ctx, "leader:probe", time.Hour)

	// Then renewal reports ownership lost, without error
	require.NoError(t, err)
	require.False(t, ok)
	require.False(t, mr.Exists("leader:probe"))
}

func TestRedisLeaderLease_RenewDoesNotResurrectDeletedKey(t *testing.T) {
	// Given a lease that was acquired and then released (key deleted)
	mr, rdb := newLeaderLeaseRedis(t)
	ctx := context.Background()
	lease := NewRedisLeaderLease(rdb, "instance-a").(service.RenewableLeaderLease)

	release, ok, err := lease.TryAcquire(ctx, "leader:probe", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	release()
	require.False(t, mr.Exists("leader:probe"))

	// When renewing after release
	ok, err = lease.Renew(ctx, "leader:probe", time.Hour)

	// Then renewal reports ownership lost and the key must NOT reappear
	require.NoError(t, err)
	require.False(t, ok)
	require.False(t, mr.Exists("leader:probe"))
}
