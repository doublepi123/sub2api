//go:build unit

package kiro

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCatalogNextAttempt_LadderAndCap(t *testing.T) {
	for i, wait := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		t.Run(fmt.Sprint(i+1), func(t *testing.T) {
			// Given
			now := time.Now()
			// When
			next, capped := CatalogNextAttempt(now, i+1, 0)
			// Then
			require.Equal(t, now.Add(wait), next)
			require.False(t, capped)
		})
	}
	for _, retry := range []time.Duration{2 * time.Hour, 99999 * time.Second} {
		t.Run(retry.String(), func(t *testing.T) {
			// Given
			now := time.Now()
			// When
			next, capped := CatalogNextAttempt(now, 1, retry)
			// Then
			require.Equal(t, now.Add(min(retry, 4*time.Hour)), next)
			require.Equal(t, retry > 4*time.Hour, capped)
		})
	}
}
