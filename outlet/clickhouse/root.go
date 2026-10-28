// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

// Package clickhouse handles flow exports to ClickHouse. This component is
// "inert" and does not track its spawned workers. It is the responsability of
// the dependent component to flush data before shutting down.
package clickhouse

import (
	"time"

	"akvorado/common/clickhousedb"
	"akvorado/common/reporter"
	"akvorado/common/schema"
)

// Component is the interface for the ClickHouse exporter component.
type Component interface {
	// NewWorker creates a worker. The onFlushed function, when not nil, is
	// called each time a batch is confirmed written to ClickHouse (or when
	// there is nothing left to write). Callers use it to commit the Kafka
	// offsets of the flows they buffered.
	NewWorker(int, *schema.FlowMessage, func()) Worker
	// FlushTimeout returns the recommended timeout for the final flush
	// of a worker on shutdown. Callers should use it as the deadline
	// for the context passed to Worker.Flush(): the worker honors the
	// grace period on its own, but only while the provided context is
	// still alive.
	FlushTimeout() time.Duration
}

// realComponent implements the ClickHouse exporter
type realComponent struct {
	r      *reporter.Reporter
	d      *Dependencies
	config Configuration

	metrics metrics
}

// Dependencies defines the dependencies of the ClickHouse exporter
type Dependencies struct {
	ClickHouse *clickhousedb.Component
	Schema     *schema.Component
}

// New creates a new clickhouse component.
func New(r *reporter.Reporter, configuration Configuration, dependencies Dependencies) (Component, error) {
	configuration.minimumBatchSize = configuration.MaximumBatchSize / minimumBatchSizeDivider
	c := realComponent{
		r:      r,
		d:      &dependencies,
		config: configuration,
	}
	c.initMetrics()
	return &c, nil
}
