// SPDX-FileCopyrightText: 2024 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !release

package kafkainput

import (
	"context"
	"testing"
)

type mockComponent struct {
	config   Configuration
	incoming chan Record
	commit   CommitFunc
}

// NewMock instantiates a fake Kafka consumer that will produce messages sent on
// the returned channel.
func NewMock(t *testing.T, config Configuration) (Component, chan<- Record) {
	return NewMockWithCommit(t, config, func(Record) {})
}

// NewMockWithCommit instantiates a fake Kafka consumer that will produce
// messages sent on the returned channel and report flushed records to the
// provided commit function.
func NewMockWithCommit(_ *testing.T, config Configuration, commit CommitFunc) (Component, chan<- Record) {
	c := mockComponent{
		config:   config,
		incoming: make(chan Record),
		commit:   commit,
	}
	return &c, c.incoming
}

// StartWorkers start a set of workers to produce received messages.
func (c *mockComponent) StartWorkers(workerBuilder WorkerBuilderFunc) error {
	ch := make(chan ScaleRequest, 10)
	go func() {
		for {
			// Ignore all incoming scaling requests
			<-ch
		}
	}()
	for i := range c.config.MinWorkers {
		callback, shutdown, _ := workerBuilder(i, ch, c.commit)
		go func() {
			defer shutdown()
			for {
				record, ok := <-c.incoming
				if !ok {
					return
				}
				callback(context.Background(), record)
			}
		}()
	}
	return nil
}

// StopWorkers stop all workers.
func (c *mockComponent) StopWorkers() {
	close(c.incoming)
}

// Stop stops the mock component.
func (c *mockComponent) Stop() error {
	c.StopWorkers()
	return nil
}
