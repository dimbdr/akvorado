// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package templatecache

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"akvorado/common/helpers"
	"akvorado/common/kafka"
	"akvorado/common/pb"
	"akvorado/common/reporter"

	"github.com/netsampler/goflow2/v3/decoders/netflow"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestDisabledCacheDoesNotConnect(t *testing.T) {
	c, err := New(reporter.NewMock(t), DefaultConfiguration())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer c.Stop()
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	if _, err := c.SetTemplate(key, testTemplate()); err != nil {
		t.Fatalf("SetTemplate() error: %v", err)
	}
	if _, ok := c.GetTemplate(key); !ok {
		t.Fatal("GetTemplate() not found in disabled cache")
	}
}

func TestKafkaPreloadAndTail(t *testing.T) {
	baseTopic := fmt.Sprintf("templates-test-%d", rand.Int())
	topic := fmt.Sprintf("%s-v%d", baseTopic, pb.Version)

	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatalf("NewCluster() error: %v", err)
	}
	defer cluster.Close()

	producerConfiguration := kafka.DefaultConfiguration()
	producerConfiguration.Brokers = cluster.ListenAddrs()
	producerOpts, err := kafka.NewConfig(reporter.NewMock(t), producerConfiguration)
	if err != nil {
		t.Fatalf("NewConfig() error:\n%+v", err)
	}
	producerOpts = append(producerOpts, kgo.ProducerLinger(0))
	producer, err := kgo.NewClient(producerOpts...)
	if err != nil {
		t.Fatalf("NewClient() error:\n%+v", err)
	}
	defer producer.Close()

	produce := func(record *kgo.Record) {
		t.Helper()
		if err := producer.ProduceSync(context.Background(), record).FirstErr(); err != nil {
			t.Fatalf("ProduceSync() error: %v", err)
		}
	}
	produceEntry := func(key Key, entry Entry) {
		t.Helper()
		value, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("json.Marshal() error: %v", err)
		}
		produce(&kgo.Record{
			Topic:     topic,
			Key:       []byte(key.String()),
			Value:     value,
			Timestamp: entry.UpdatedAt,
		})
	}
	produceTombstone := func(key Key) {
		t.Helper()
		produce(&kgo.Record{
			Topic:     topic,
			Key:       []byte(key.String()),
			Value:     nil,
			Timestamp: time.Now(),
		})
	}

	config := DefaultConfiguration()
	config.Enabled = true
	config.Brokers = cluster.ListenAddrs()
	config.Topic = baseTopic
	config.PreloadTimeout = 10 * time.Second
	c := newCache(t, config)

	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	initial := Entry{
		Kind: KindData, Hash: "a", Origin: "peer", UpdatedAt: time.Now().Add(-time.Minute),
		Template: netflow.TemplateRecord{TemplateId: 256, FieldCount: 1},
	}
	produceEntry(key, initial)

	if err := c.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer c.Stop()

	if got, ok := c.GetTemplate(key); !ok {
		t.Fatal("preload did not find the template")
	} else if diff := helpers.Diff(got, initial.Template); diff != "" {
		t.Fatalf("preload (-got, +want):\n%s", diff)
	}

	// The tail applies a newer peer record.
	updated := Entry{
		Kind: KindData, Hash: "b", Origin: "peer", UpdatedAt: time.Now(),
		Template: netflow.TemplateRecord{TemplateId: 256, FieldCount: 2},
	}
	produceEntry(key, updated)
	waitFor(t, 2*time.Second, func() bool {
		got, ok := c.GetTemplate(key)
		return ok && helpers.Diff(got, updated.Template) == ""
	})

	// A tombstone removes the entry.
	produceTombstone(key)
	waitFor(t, 2*time.Second, func() bool {
		_, ok := c.GetTemplate(key)
		return !ok
	})

	// A template, a tombstone and a template: the final state is the template,
	// even though its updated_at is older than the tombstone. Tombstones are
	// applied in log order, not by timestamp.
	first := Entry{
		Kind: KindData, Hash: "c", Origin: "peer", UpdatedAt: time.Now().Add(-2 * time.Hour),
		Template: netflow.TemplateRecord{TemplateId: 256, FieldCount: 3},
	}
	last := Entry{
		Kind: KindData, Hash: "d", Origin: "peer", UpdatedAt: time.Now().Add(-time.Hour),
		Template: netflow.TemplateRecord{TemplateId: 256, FieldCount: 4},
	}
	produceEntry(key, first)
	produceTombstone(key)
	produceEntry(key, last)
	waitFor(t, 2*time.Second, func() bool {
		got, ok := c.GetTemplate(key)
		return ok && helpers.Diff(got, last.Template) == ""
	})
}

func TestPublishDoesNotBlock(t *testing.T) {
	r := reporter.NewMock(t)
	config := DefaultConfiguration()
	config.Enabled = true
	c, err := New(r, config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	c.producer = &fakeProducer{accept: false} // buffer full
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	done := make(chan struct{})
	go func() {
		c.SetTemplate(key, testTemplate())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SetTemplate() blocked on a full producer buffer")
	}
	metrics := r.GetMetrics("akvorado_outlet_flow_templatecache_", "publish_dropped_total")
	if got := metrics["publish_dropped_total"]; got != "1" {
		t.Fatalf("publish_dropped = %q, want \"1\"", got)
	}
}

func TestOwnTombstoneEchoIgnored(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	p := &fakeProducer{accept: true}
	c.producer = p
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	c.SetTemplate(key, testTemplate()) // p.records[0]
	c.DeleteTemplate(key)              // p.records[1], the tombstone
	c.SetTemplate(key, testTemplate()) // p.records[2], the re-add
	if len(p.records) != 3 {
		t.Fatalf("published %d records, want 3", len(p.records))
	}
	if got := string(p.records[1].Headers[0].Value); got != c.origin {
		t.Fatalf("tombstone origin = %q, want %q", got, c.origin)
	}
	c.applyRecord(p.records[1]) // the pod's own tombstone echo
	if _, ok := c.GetTemplate(key); !ok {
		t.Fatal("own tombstone echo deleted the re-add")
	}
}

func TestKafkaUnreachableDoesNotBlockStart(t *testing.T) {
	config := DefaultConfiguration()
	config.Enabled = true
	config.Brokers = []string{"127.0.0.1:1"}
	config.PreloadTimeout = 500 * time.Millisecond
	c := newCache(t, config)
	done := make(chan struct{})
	go func() { c.Start(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return within the preload timeout")
	}
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	c.SetTemplate(key, testTemplate())
	c.Stop()
}

func TestTopicMissingIsEmptyCache(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatalf("NewCluster() error: %v", err)
	}
	defer cluster.Close()
	config := DefaultConfiguration()
	config.Enabled = true
	config.Brokers = cluster.ListenAddrs()
	config.Topic = "missing"
	config.PreloadTimeout = 10 * time.Second
	c := newCache(t, config)
	start := time.Now()
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Start() took %s for a missing topic, want < 2s", elapsed)
	}
	c.Stop()
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition not met before timeout")
	}
}
