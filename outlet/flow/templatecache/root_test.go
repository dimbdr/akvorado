// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package templatecache

import (
	"context"
	"sync"
	"testing"
	"time"

	"akvorado/common/helpers"
	"akvorado/common/kafka"
	"akvorado/common/reporter"

	"github.com/netsampler/goflow2/v3/decoders/netflow"
	"github.com/twmb/franz-go/pkg/kgo"
)

func testTemplate() netflow.TemplateRecord {
	return netflow.TemplateRecord{TemplateId: 256, FieldCount: 1, Fields: []netflow.Field{{Type: 8, Length: 4}}}
}

func newCache(t *testing.T, config Configuration) *Cache {
	t.Helper()
	c, err := New(reporter.NewMock(t), config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return c
}

func TestConfigurationDefaults(t *testing.T) {
	c := newCache(t, Configuration{Enabled: true, Configuration: kafka.Configuration{Topic: "custom"}})
	if c.config.QueueSize < 1 {
		t.Fatalf("QueueSize = %d, want >= 1", c.config.QueueSize)
	}
	if c.config.RefreshInterval <= 0 || c.config.PreloadTimeout <= 0 {
		t.Fatalf("durations = %s/%s, want > 0", c.config.RefreshInterval, c.config.PreloadTimeout)
	}
	if c.config.Topic != "custom" {
		t.Fatalf("Topic = %q, want custom", c.config.Topic)
	}
}

func TestSetGetDelete(t *testing.T) {
	c, err := New(reporter.NewMock(t), DefaultConfiguration())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	added, err := c.SetTemplate(key, testTemplate())
	if err != nil || !added {
		t.Fatalf("SetTemplate() = %v, %v; want true, nil", added, err)
	}
	got, ok := c.GetTemplate(key)
	if !ok {
		t.Fatal("GetTemplate() not found")
	}
	if diff := helpers.Diff(got, testTemplate()); diff != "" {
		t.Fatalf("GetTemplate() (-got, +want):\n%s", diff)
	}
	if err := c.DeleteTemplate(key); err != nil {
		t.Fatalf("DeleteTemplate() error: %v", err)
	}
	if _, ok := c.GetTemplate(key); ok {
		t.Fatal("GetTemplate() found after delete")
	}
}

func TestUpdatedAtWins(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	newer := Entry{Kind: "data", Hash: "b", UpdatedAt: time.Now(), Template: netflow.TemplateRecord{TemplateId: 256, FieldCount: 2}}
	older := Entry{Kind: "data", Hash: "a", UpdatedAt: time.Now().Add(-time.Hour), Template: netflow.TemplateRecord{TemplateId: 256, FieldCount: 1}}
	c.set(key, newer, sourceRestore)
	c.set(key, older, sourceRestore)
	got, ok := c.GetTemplate(key)
	if !ok {
		t.Fatal("GetTemplate() not found")
	}
	if diff := helpers.Diff(got, newer.Template); diff != "" {
		t.Fatalf("GetTemplate() kept the older entry (-got, +want):\n%s", diff)
	}
}

func TestTTL(t *testing.T) {
	r := reporter.NewMock(t)
	config := DefaultConfiguration()
	config.TTL = time.Hour
	c, err := New(r, config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	// Restore keeps the expired entry: it is a fast path, not a filter.
	// GetTemplate treats it as a miss and purge removes it.
	c.Restore(map[Key]Entry{key: {
		Kind:      "data",
		Hash:      "a",
		UpdatedAt: time.Now().Add(-2 * time.Hour),
		Template:  testTemplate(),
	}})
	if _, ok := c.GetTemplate(key); ok {
		t.Fatal("GetTemplate() found an expired entry")
	}
	c.purge()
	metrics := r.GetMetrics("akvorado_outlet_flow_templatecache_", "expired_total")
	if got := metrics["expired_total"]; got != "1" {
		t.Fatalf("expired counter = %q, want \"1\"", got)
	}
}

func TestDeleteAbsentDoesNotPublish(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	p := &fakeProducer{accept: true}
	c.producer = p
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	if err := c.DeleteTemplate(key); err != nil {
		t.Fatalf("DeleteTemplate() error: %v", err)
	}
	if len(p.records) != 0 {
		t.Fatalf("DeleteTemplate() published %d records for an absent key", len(p.records))
	}
}

func TestDeleteTemplates(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	exporter := "192.0.2.10"
	dataKey := Key{Version: 9, Exporter: exporter, ObsDomainID: 300, TemplateID: 256}
	optionsKey := Key{Version: 9, Exporter: exporter, ObsDomainID: 300, TemplateID: 257}
	c.SetTemplate(dataKey, testTemplate())
	c.SetTemplate(optionsKey, netflow.NFv9OptionsTemplateRecord{TemplateId: 257, ScopeLength: 4, OptionLength: 4})
	if err := c.DeleteTemplates(exporter, 9, 300, false); err != nil {
		t.Fatalf("DeleteTemplates() error: %v", err)
	}
	if _, ok := c.GetTemplate(dataKey); ok {
		t.Fatal("data template still present")
	}
	if _, ok := c.GetTemplate(optionsKey); !ok {
		t.Fatal("options template was deleted with the data templates")
	}
}

func TestSamplingRateDedupe(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	p := &fakeProducer{accept: true}
	c.producer = p
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, SamplerID: 10, IsSampler: true}
	if err := c.SetSamplingRate(key, 100); err != nil {
		t.Fatalf("SetSamplingRate() error: %v", err)
	}
	if err := c.SetSamplingRate(key, 100); err != nil {
		t.Fatalf("SetSamplingRate() error: %v", err)
	}
	if len(p.records) != 1 {
		t.Fatalf("same sampling rate published %d records, want 1", len(p.records))
	}
	if err := c.SetSamplingRate(key, 200); err != nil {
		t.Fatalf("SetSamplingRate() error: %v", err)
	}
	if len(p.records) != 2 {
		t.Fatalf("changed sampling rate published %d records, want 2", len(p.records))
	}
}

func TestChangedTemplatePublishesImmediately(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	p := &fakeProducer{accept: true}
	c.producer = p
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	if _, err := c.SetTemplate(key, testTemplate()); err != nil {
		t.Fatalf("SetTemplate() error: %v", err)
	}
	changed := netflow.TemplateRecord{TemplateId: 256, FieldCount: 2}
	if _, err := c.SetTemplate(key, changed); err != nil {
		t.Fatalf("SetTemplate() error: %v", err)
	}
	if len(p.records) != 2 {
		t.Fatalf("changed template published %d records, want 2 immediately", len(p.records))
	}
}

func TestUnchangedTemplateRefreshesAfterInterval(t *testing.T) {
	config := DefaultConfiguration()
	config.RefreshInterval = 10 * time.Millisecond
	c := newCache(t, config)
	p := &fakeProducer{accept: true}
	c.producer = p
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	if _, err := c.SetTemplate(key, testTemplate()); err != nil {
		t.Fatalf("SetTemplate() error: %v", err)
	}
	if _, err := c.SetTemplate(key, testTemplate()); err != nil {
		t.Fatalf("SetTemplate() error: %v", err)
	}
	if len(p.records) != 1 {
		t.Fatalf("unchanged template published %d records before the interval, want 1", len(p.records))
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := c.SetTemplate(key, testTemplate()); err != nil {
		t.Fatalf("SetTemplate() error: %v", err)
	}
	if len(p.records) != 2 {
		t.Fatalf("unchanged template published %d records after the interval, want 2", len(p.records))
	}
}

func TestOwnEchoIgnored(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	if _, err := c.SetTemplate(key, testTemplate()); err != nil {
		t.Fatalf("SetTemplate() error: %v", err)
	}
	echo := Entry{
		Kind: "data", Hash: "echo", Origin: c.origin, UpdatedAt: time.Now().Add(time.Hour),
		Template: netflow.TemplateRecord{TemplateId: 256, FieldCount: 9},
	}
	c.set(key, echo, sourceKafka)
	got, ok := c.GetTemplate(key)
	if !ok {
		t.Fatal("GetTemplate() not found after own echo")
	}
	if diff := helpers.Diff(got, testTemplate()); diff != "" {
		t.Fatalf("own echo overwrote the local template (-got, +want):\n%s", diff)
	}
}

func TestLocalWinsOverPeerClock(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	key := Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}
	if _, err := c.SetTemplate(key, testTemplate()); err != nil {
		t.Fatalf("SetTemplate() error: %v", err)
	}
	peer := Entry{
		Kind: "data", Hash: "peer", Origin: "peer", UpdatedAt: time.Now().Add(-time.Hour),
		Template: netflow.TemplateRecord{TemplateId: 256, FieldCount: 9},
	}
	c.set(key, peer, sourceKafka)
	got, ok := c.GetTemplate(key)
	if !ok {
		t.Fatal("GetTemplate() not found after an older peer record")
	}
	if diff := helpers.Diff(got, testTemplate()); diff != "" {
		t.Fatalf("an older peer record overwrote the local template (-got, +want):\n%s", diff)
	}
}

func TestDeleteTemplatesSkipsSamplers(t *testing.T) {
	c := newCache(t, DefaultConfiguration())
	exporter := "192.0.2.10"
	samplerKey := Key{Version: 9, Exporter: exporter, ObsDomainID: 300, SamplerID: 10, IsSampler: true}
	if err := c.SetSamplingRate(samplerKey, 100); err != nil {
		t.Fatalf("SetSamplingRate() error: %v", err)
	}
	if err := c.DeleteTemplates(exporter, 9, 300, false); err != nil {
		t.Fatalf("DeleteTemplates() error: %v", err)
	}
	if _, ok := c.GetSamplingRate(samplerKey); !ok {
		t.Fatal("sampler key was deleted with the data templates")
	}
}

type fakeProducer struct {
	mu      sync.Mutex
	records []*kgo.Record
	accept  bool
}

func (p *fakeProducer) TryProduce(_ context.Context, r *kgo.Record, promise func(*kgo.Record, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.accept {
		if promise != nil {
			promise(r, kgo.ErrMaxBuffered)
		}
		return
	}
	p.records = append(p.records, r)
	if promise != nil {
		promise(r, nil)
	}
}

func (p *fakeProducer) Close() {}
