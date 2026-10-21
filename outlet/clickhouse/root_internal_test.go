// SPDX-FileCopyrightText: 2026 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse

import (
	"testing"
	"time"
)

func TestFlushTimeout(t *testing.T) {
	config := DefaultConfiguration()
	c := realComponent{config: config}
	// FlushTimeout has to exceed the grace period: the worker waits for
	// the grace period once its parent context is done, so a parent
	// deadline equal to the grace period would cancel the last retry
	// before it can use the full grace period.
	if got := c.FlushTimeout(); got <= config.GracePeriod {
		t.Fatalf("FlushTimeout() = %s, want strictly more than the grace period (%s)", got, config.GracePeriod)
	}

	config.GracePeriod = 3 * time.Minute
	c.config = config
	if got, want := c.FlushTimeout(), 3*time.Minute+10*time.Second; got != want {
		t.Fatalf("FlushTimeout() = %s, want %s", got, want)
	}
}
