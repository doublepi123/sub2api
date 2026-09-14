//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNoopLeaderLease_AlwaysAcquires(t *testing.T) {
	// Given a noop leader lease (single-process / no-Redis fallback)
	lease := NoopLeaderLease()

	// When acquiring the same key twice in a row
	release1, ok1, err1 := lease.TryAcquire(context.Background(), "leader:test", time.Minute)
	release2, ok2, err2 := lease.TryAcquire(context.Background(), "leader:test", time.Minute)

	// Then both acquisitions succeed without error
	require.NoError(t, err1)
	require.True(t, ok1)
	require.NotNil(t, release1)
	require.NoError(t, err2)
	require.True(t, ok2)
	require.NotNil(t, release2)

	// And release is safe to call exactly once per acquisition (idempotent no-op)
	require.NotPanics(t, func() {
		release1()
		release2()
	})
}
