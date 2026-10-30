// SPDX-FileCopyrightText: 2026 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package kafkainput

import (
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"

	"akvorado/common/helpers"
	"akvorado/common/reporter"
)

// TestOnPartitionsRevokedForgets checks the revoke callback both commits the
// offsets already marked and tells the worker to forget the revoked
// partitions: a stale position committed after the partition moved to another
// member would move the group's committed offset backwards.
func TestOnPartitionsRevokedForgets(t *testing.T) {
	client, err := kgo.NewClient(kgo.SeedBrokers("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("kgo.NewClient() error:\n%+v", err)
	}
	defer client.Close()

	var got map[string][]int32
	callback := onPartitionsRevoked(reporter.NewMock(t).With().Logger(), func(revoked map[string][]int32) {
		got = revoked
	})
	revoked := map[string][]int32{"raw-flows": {1, 2}}
	callback(t.Context(), client, revoked)
	if diff := helpers.Diff(got, revoked); diff != "" {
		t.Errorf("revoked partitions (-got, +want):\n%s", diff)
	}
}
