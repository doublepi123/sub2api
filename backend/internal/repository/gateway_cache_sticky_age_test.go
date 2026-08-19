package repository

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStickyBindingUnixPastMaxAge(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	maxAge := time.Hour

	require.False(t, stickyBindingUnixPastMaxAge("", now, maxAge))
	require.False(t, stickyBindingUnixPastMaxAge("not-a-number", now, maxAge))
	require.False(t, stickyBindingUnixPastMaxAge("0", now, maxAge))
	require.False(t, stickyBindingUnixPastMaxAge("-1", now, maxAge))
	require.False(t, stickyBindingUnixPastMaxAge("1700000000", now, 0))

	require.False(t, stickyBindingUnixPastMaxAge("1700000000", now.Add(maxAge-time.Second), maxAge))
	require.True(t, stickyBindingUnixPastMaxAge("1700000000", now.Add(maxAge), maxAge))
	require.True(t, stickyBindingUnixPastMaxAge("1700000000", now.Add(maxAge+time.Second), maxAge))
}

func TestBuildSessionBoundAtKeyIsolatesGroup(t *testing.T) {
	require.Equal(t, "sticky_session_bound_at:1:abc", buildSessionBoundAtKey(1, "abc"))
	require.NotEqual(t, buildSessionBoundAtKey(1, "abc"), buildSessionBoundAtKey(2, "abc"))
	require.NotEqual(t, buildSessionKey(1, "abc"), buildSessionBoundAtKey(1, "abc"))
}
