// SPDX-FileCopyrightText: 2026 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package flow

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"akvorado/common/helpers"
	"akvorado/common/pb"
	"akvorado/common/reporter"
	"akvorado/common/schema"
	"akvorado/outlet/flow/decoder"
	"akvorado/outlet/flow/templatecache"

	"github.com/twmb/franz-go/pkg/kfake"
)

// waitFor polls condition every 50ms until it holds or the timeout expires.
func waitFor(t *testing.T, description string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", description)
}

// newOutlet builds a flow component wired to the provided template topic, as
// the orchestrator would configure it.
func newOutlet(t *testing.T, baseTopic string, brokers []string) (*Component, *reporter.Reporter, *schema.Component) {
	t.Helper()
	r := reporter.NewMock(t)
	sch := schema.NewMock(t)
	tc := templatecache.DefaultConfiguration()
	tc.Enabled = true
	tc.Brokers = brokers
	tc.Topic = baseTopic
	c, err := New(r, Configuration{TemplateCache: &tc}, Dependencies{Schema: sch})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	return c, r, sch
}

// decodePacket decodes one NetFlow/IPFIX packet on the outlet and returns the
// number of flows it produced.
func decodePacket(t *testing.T, c *Component, sch *schema.Component, payload []byte) (int, error) {
	t.Helper()
	bf := sch.NewFlowMessage()
	nb := 0
	finalize := func() {
		nb++
		bf.Clear()
	}
	err := c.Decode(&pb.RawFlow{
		TimeReceived:     uint64(time.Now().Unix()),
		Payload:          payload,
		SourceAddress:    helpers.AddrTo6(netip.MustParseAddr("::ffff:127.0.0.1")).AsSlice(),
		Decoder:          pb.RawFlow_DECODER_NETFLOW,
		TimestampSource:  pb.RawFlow_TS_INPUT,
		UseSourceAddress: true,
	}, bf, finalize)
	return nb, err
}

// TestSharedTemplateCacheEndToEnd wires two outlets to the same compacted
// topic, as the orchestrator does, and checks a template published by one is
// used by the other, including withdrawal, withdraw-all and sampling rates.
func TestSharedTemplateCacheEndToEnd(t *testing.T) {
	baseTopic := fmt.Sprintf("templates-test-%d", rand.Int())
	topic := fmt.Sprintf("%s-v%d", baseTopic, pb.Version)
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatalf("NewCluster() error: %v", err)
	}
	defer cluster.Close()

	a, ar, sch := newOutlet(t, baseTopic, cluster.ListenAddrs())
	defer a.Stop()
	b, br, bsch := newOutlet(t, baseTopic, cluster.ListenAddrs())
	defer b.Stop()

	packets := slices.Collect(helpers.ReadManyPcapL4(t, filepath.Join("decoder", "netflow", "testdata", "datalink.pcap")))
	if len(packets) < 2 {
		t.Fatalf("datalink.pcap has %d packets, want at least 2", len(packets))
	}
	templatePacket, dataPacket := packets[0], packets[1]
	key := templatecache.Key{
		Version:     10,
		Exporter:    "::ffff:127.0.0.1",
		ObsDomainID: 16843264,
		TemplateID:  384,
	}

	// Outlet A receives the template. It is stored locally and published.
	if nb, err := decodePacket(t, a, sch, templatePacket); err != nil || nb != 0 {
		t.Fatalf("template decode = %d, %v; want 0, nil", nb, err)
	}
	if _, ok := a.cache.GetTemplate(key); !ok {
		t.Fatalf("outlet A did not store the template")
	}

	// Outlet B learns it from the topic and decodes the data-only packet.
	waitFor(t, "outlet B to decode the data packet from the topic", 5*time.Second, func() bool {
		nb, err := decodePacket(t, b, bsch, dataPacket)
		return err == nil && nb == 1
	})

	// A withdrawal published by A removes the template from B.
	if err := a.cache.DeleteTemplate(key); err != nil {
		t.Fatalf("DeleteTemplate() error: %v", err)
	}
	waitFor(t, "outlet B to see the template withdrawal", 5*time.Second, func() bool {
		_, err := decodePacket(t, b, bsch, dataPacket)
		var notFound *decoder.TemplateNotFoundError
		return errors.As(err, &notFound)
	})

	// A re-add published by A restores it on B.
	if nb, err := decodePacket(t, a, sch, templatePacket); err != nil || nb != 0 {
		t.Fatalf("template re-decode = %d, %v; want 0, nil", nb, err)
	}
	waitFor(t, "outlet B to see the re-added template", 5*time.Second, func() bool {
		nb, err := decodePacket(t, b, bsch, dataPacket)
		return err == nil && nb == 1
	})

	// IPFIX withdraw-all removes every data template for the exporter and
	// observation domain, on B too.
	if err := a.cache.DeleteTemplates("::ffff:127.0.0.1", 10, 16843264, false); err != nil {
		t.Fatalf("DeleteTemplates() error: %v", err)
	}
	waitFor(t, "outlet B to see the withdraw-all", 5*time.Second, func() bool {
		_, err := decodePacket(t, b, bsch, dataPacket)
		var notFound *decoder.TemplateNotFoundError
		return errors.As(err, &notFound)
	})

	// Sampling rates travel over the same topic: this is the startup loss where
	// a pod skipped flows because another pod had seen the sampler.
	samplerKey := templatecache.Key{
		Version:     10,
		Exporter:    "::ffff:127.0.0.1",
		ObsDomainID: 16843264,
		SamplerID:   10,
		IsSampler:   true,
	}
	if err := a.cache.SetSamplingRate(samplerKey, 1000); err != nil {
		t.Fatalf("SetSamplingRate() error: %v", err)
	}
	waitFor(t, "outlet B to receive the sampling rate", 5*time.Second, func() bool {
		rate, ok := b.cache.GetSamplingRate(samplerKey)
		return ok && rate == 1000
	})

	// A publishes; B tails. Do not assert A's received_total (A consumes its
	// own echo) nor B's published_total.
	if got := ar.GetMetrics("akvorado_outlet_flow_templatecache_", "published_total")["published_total"]; got == "" || got == "0" {
		t.Fatalf("outlet A published_total = %q, want > 0", got)
	}
	if got := br.GetMetrics("akvorado_outlet_flow_templatecache_", "received_total")["received_total"]; got == "" || got == "0" {
		t.Fatalf("outlet B received_total = %q, want > 0", got)
	}
}

// TestSharedTemplateCacheKafkaDownDoesNotBlock checks an unreachable broker
// never blocks the outlet: Start is bounded by PreloadTimeout and decoding does
// not wait for Kafka.
func TestSharedTemplateCacheKafkaDownDoesNotBlock(t *testing.T) {
	r := reporter.NewMock(t)
	sch := schema.NewMock(t)
	tc := templatecache.DefaultConfiguration()
	tc.Enabled = true
	tc.Brokers = []string{"127.0.0.1:1"}
	tc.PreloadTimeout = 500 * time.Millisecond
	c, err := New(r, Configuration{TemplateCache: &tc}, Dependencies{Schema: sch})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	start := time.Now()
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer c.Stop()
	// Start blocks for the whole PreloadTimeout while kadm retries; the 2s bound
	// matches TestKafkaUnreachableDoesNotBlockStart.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Start() took %s with an unreachable broker, want < 2s", elapsed)
	}

	packets := slices.Collect(helpers.ReadManyPcapL4(t, filepath.Join("decoder", "netflow", "testdata", "datalink.pcap")))
	done := make(chan struct{})
	go func() {
		decodePacket(t, c, sch, packets[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Decode() blocked on an unreachable broker")
	}
}
