// SPDX-FileCopyrightText: 2026 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"bytes"
	"context"
	"encoding/gob"
	"net/netip"
	"sort"
	"testing"
	"time"

	"akvorado/common/constants"
	"akvorado/common/daemon"
	"akvorado/common/helpers"
	"akvorado/common/httpserver"
	"akvorado/common/pb"
	"akvorado/common/reporter"
	"akvorado/common/schema"
	"akvorado/outlet/clickhouse"
	"akvorado/outlet/flow"
	"akvorado/outlet/kafkainput"
	"akvorado/outlet/metadata"
	"akvorado/outlet/routing"
)

// TestCommitTracker checks the tracker only hands out the latest position of
// each partition, once, and only when a flush is reported.
func TestCommitTracker(t *testing.T) {
	var got []kafkainput.Record
	tracker := newCommitTracker(func(record kafkainput.Record) {
		got = append(got, record)
	})

	// Nothing tracked, nothing to commit.
	tracker.flushed()
	if len(got) != 0 {
		t.Fatalf("flushed() committed %d records without any tracked record", len(got))
	}

	// Two records on the same partition: only the latest position matters.
	tracker.track(kafkainput.Record{Topic: "t", Partition: 0, Offset: 10})
	tracker.track(kafkainput.Record{Topic: "t", Partition: 0, Offset: 11})
	// A record on another partition.
	tracker.track(kafkainput.Record{Topic: "t", Partition: 1, Offset: 4})

	tracker.flushed()
	sort.Slice(got, func(i, j int) bool {
		if got[i].Partition != got[j].Partition {
			return got[i].Partition < got[j].Partition
		}
		return got[i].Offset < got[j].Offset
	})
	expected := []kafkainput.Record{
		{Topic: "t", Partition: 0, Offset: 11},
		{Topic: "t", Partition: 1, Offset: 4},
	}
	if diff := helpers.Diff(got, expected); diff != "" {
		t.Errorf("flushed() commits (-got, +want):\n%s", diff)
	}

	// A second flush without new records must not commit anything again.
	got = nil
	tracker.flushed()
	if len(got) != 0 {
		t.Errorf("flushed() committed %d records twice", len(got))
	}
}

// TestCommitTrackerForget checks a revoked partition is dropped from the
// tracker: after a rebalance another member owns it, and committing our stale
// position would move the group's committed offset backwards.
func TestCommitTrackerForget(t *testing.T) {
	var got []kafkainput.Record
	tracker := newCommitTracker(func(record kafkainput.Record) {
		got = append(got, record)
	})

	tracker.track(kafkainput.Record{Topic: "t", Partition: 0, Offset: 5})
	tracker.track(kafkainput.Record{Topic: "t", Partition: 1, Offset: 9})
	tracker.forget(map[string][]int32{"t": {0}})
	tracker.flushed()

	expected := []kafkainput.Record{{Topic: "t", Partition: 1, Offset: 9}}
	if diff := helpers.Diff(got, expected); diff != "" {
		t.Errorf("flushed() commits after forgetting a partition (-got, +want):\n%s", diff)
	}
}

// TestCommitTrackerOlderRecordDoesNotRegress checks a late record on a
// partition cannot move the committed position backwards.
func TestCommitTrackerOlderRecordDoesNotRegress(t *testing.T) {
	var got []kafkainput.Record
	tracker := newCommitTracker(func(record kafkainput.Record) {
		got = append(got, record)
	})

	tracker.track(kafkainput.Record{Topic: "t", Partition: 0, Offset: 11})
	tracker.track(kafkainput.Record{Topic: "t", Partition: 0, Offset: 9})
	tracker.flushed()

	expected := []kafkainput.Record{{Topic: "t", Partition: 0, Offset: 11}}
	if diff := helpers.Diff(got, expected); diff != "" {
		t.Errorf("flushed() commits (-got, +want):\n%s", diff)
	}
}

// flushableClickHouse is a ClickHouse mock whose workers keep the flows
// buffered until Flush is called, and report the flush through the provided
// hook. This lets a test control when a batch becomes durable.
type flushableClickHouse struct {
	workers []*flushableWorker
}

func (c *flushableClickHouse) NewWorker(_ int, bf *schema.FlowMessage, onFlushed func()) clickhouse.Worker {
	w := &flushableWorker{bf: bf, onFlushed: onFlushed}
	c.workers = append(c.workers, w)
	return w
}

func (c *flushableClickHouse) FlushTimeout() time.Duration { return time.Second }

// worker returns the first worker.
func (c *flushableClickHouse) worker() *flushableWorker {
	return c.workers[0]
}

type flushableWorker struct {
	bf        *schema.FlowMessage
	onFlushed func()
}

func (w *flushableWorker) FinalizeAndSend(context.Context) clickhouse.WorkerStatus {
	// Like the real worker, finalize the flow so it is part of the batch, but
	// keep it buffered until Flush is called.
	w.bf.Finalize()
	return clickhouse.WorkerStatusIdle
}

func (w *flushableWorker) Flush(context.Context) {
	w.bf.Clear()
	if w.onFlushed != nil {
		w.onFlushed()
	}
}

func (w *flushableWorker) Close() {}

// newCoreForCommitTest builds a core component wired to the provided ClickHouse
// component, with every other dependency mocked, so a test can drive one worker
// synchronously.
func newCoreForCommitTest(t *testing.T, ch clickhouse.Component) *Component {
	t.Helper()
	r := reporter.NewMock(t)
	daemonComponent := daemon.NewMock(t)
	metadataComponent := metadata.NewMock(t, r, metadata.DefaultConfiguration(),
		metadata.Dependencies{Daemon: daemonComponent})
	flowComponent, err := flow.New(r, flow.DefaultConfiguration(), flow.Dependencies{Schema: schema.NewMock(t)})
	if err != nil {
		t.Fatalf("flow.New() error:\n%+v", err)
	}
	httpComponent := httpserver.NewMock(t, r)
	routingComponent := routing.NewMock(t, r)
	routingComponent.PopulateRIB(t)
	kafkaInputComponent, _ := kafkainput.NewMock(t, kafkainput.DefaultConfiguration())

	c, err := New(r, DefaultConfiguration(), Dependencies{
		Daemon:     daemonComponent,
		Flow:       flowComponent,
		Metadata:   metadataComponent,
		KafkaInput: kafkaInputComponent,
		ClickHouse: ch,
		HTTP:       httpComponent,
		Routing:    routingComponent,
		Schema:     schema.NewMock(t),
	})
	if err != nil {
		t.Fatalf("New() error:\n%+v", err)
	}
	return c
}

// makeRecord builds a valid raw-flow Kafka record (one GOB-encoded flow).
func makeRecord(t *testing.T) kafkainput.Record {
	t.Helper()
	msg := &schema.FlowMessage{
		TimeReceived:    200,
		SamplingRate:    1000,
		ExporterAddress: helpers.AddrTo6(netip.MustParseAddr("192.0.2.142")),
		InIf:            434,
		OutIf:           677,
		SrcAddr:         netip.MustParseAddr("::ffff:67.43.156.77"),
		DstAddr:         netip.MustParseAddr("::ffff:2.125.160.216"),
		OtherColumns: map[schema.ColumnKey]any{
			schema.ColumnBytes:   uint64(6765),
			schema.ColumnPackets: uint64(4),
			schema.ColumnEType:   uint32(0x800),
			schema.ColumnProto:   uint32(constants.ProtoTCP),
			schema.ColumnSrcPort: uint16(8534),
			schema.ColumnDstPort: uint16(80),
		},
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
		t.Fatalf("gob.Encode() error:\n%+v", err)
	}
	rawFlow := &pb.RawFlow{
		TimeReceived:    uint64(time.Now().Unix()),
		Payload:         buf.Bytes(),
		SourceAddress:   msg.ExporterAddress.AsSlice(),
		Decoder:         pb.RawFlow_DECODER_GOB,
		TimestampSource: pb.RawFlow_TS_INPUT,
	}
	data, err := rawFlow.MarshalVT()
	if err != nil {
		t.Fatalf("MarshalVT() error:\n%+v", err)
	}
	return kafkainput.Record{Topic: "raw-flows", Partition: 3, Offset: 42, Value: data}
}

// eagerClickHouse flushes as soon as a flow is finalized, mimicking a worker
// whose batch fills up in the middle of decoding a record.
type eagerClickHouse struct {
	workers []*eagerWorker
}

func (c *eagerClickHouse) NewWorker(_ int, bf *schema.FlowMessage, onFlushed func()) clickhouse.Worker {
	w := &eagerWorker{bf: bf, onFlushed: onFlushed}
	c.workers = append(c.workers, w)
	return w
}

func (c *eagerClickHouse) FlushTimeout() time.Duration { return time.Second }

func (c *eagerClickHouse) worker() *eagerWorker { return c.workers[0] }

type eagerWorker struct {
	bf        *schema.FlowMessage
	onFlushed func()
	flushed   bool
}

func (w *eagerWorker) FinalizeAndSend(context.Context) clickhouse.WorkerStatus {
	w.bf.Finalize()
	if !w.flushed {
		w.flushed = true
		w.bf.Clear()
		if w.onFlushed != nil {
			w.onFlushed()
		}
	}
	return clickhouse.WorkerStatusIdle
}

func (w *eagerWorker) Flush(context.Context) {
	w.bf.Clear()
	if w.onFlushed != nil {
		w.onFlushed()
	}
}

func (w *eagerWorker) Close() {}

// TestOffsetsCommittedAfterFlush wires the core worker to a controllable
// ClickHouse worker and checks a Kafka offset is only committed once the batch
// containing the record has been flushed.
func TestOffsetsCommittedAfterFlush(t *testing.T) {
	clickhouseComponent := &flushableClickHouse{}
	c := newCoreForCommitTest(t, clickhouseComponent)

	// Process one record synchronously.
	var commits []kafkainput.Record
	receiveFunc, shutdown, _ := c.newWorker(0, make(chan kafkainput.ScaleRequest, 10),
		func(record kafkainput.Record) {
			commits = append(commits, record)
		})
	defer shutdown()

	record := makeRecord(t)
	if err := receiveFunc(t.Context(), record); err != nil {
		t.Fatalf("receiveFunc() error:\n%+v", err)
	}

	// The flow is buffered, not flushed: its offset must not be committed.
	if diff := helpers.Diff(clickhouseComponent.worker().bf.FlowCount(), 1); diff != "" {
		t.Fatalf("buffered flows (-got, +want):\n%s", diff)
	}
	if len(commits) != 0 {
		t.Fatalf("offset committed before the batch was flushed: %+v", commits)
	}

	// Once the batch is flushed, the offset is committed.
	clickhouseComponent.worker().Flush(t.Context())
	expected := []kafkainput.Record{{Topic: "raw-flows", Partition: 3, Offset: 42}}
	if diff := helpers.Diff(commits, expected); diff != "" {
		t.Errorf("committed records (-got, +want):\n%s", diff)
	}

	// A second flush without new records must not commit the record again.
	clickhouseComponent.worker().Flush(t.Context())
	if diff := helpers.Diff(commits, expected); diff != "" {
		t.Errorf("committed records after a second flush (-got, +want):\n%s", diff)
	}
}

// TestUndecodableRecordIsSkipped checks a record that cannot be decoded does
// not stop the worker: it is counted, its offset is tracked and committed once
// the batch is durable. Returning an error here would stop the component and
// re-read the same record forever after a restart.
func TestUndecodableRecordIsSkipped(t *testing.T) {
	clickhouseComponent := &flushableClickHouse{}
	c := newCoreForCommitTest(t, clickhouseComponent)

	var commits []kafkainput.Record
	receiveFunc, shutdown, _ := c.newWorker(0, make(chan kafkainput.ScaleRequest, 10),
		func(record kafkainput.Record) {
			commits = append(commits, record)
		})
	defer shutdown()

	record := kafkainput.Record{Topic: "raw-flows", Partition: 1, Offset: 7, Value: []byte("not a protobuf")}
	if err := receiveFunc(t.Context(), record); err != nil {
		t.Fatalf("receiveFunc() error:\n%+v\nan undecodable record must not stop the worker", err)
	}
	if diff := helpers.Diff(clickhouseComponent.worker().bf.FlowCount(), 0); diff != "" {
		t.Errorf("buffered flows after an undecodable record (-got, +want):\n%s", diff)
	}

	// The offset is still tracked, so the next flush commits (skips) it.
	clickhouseComponent.worker().Flush(t.Context())
	expected := []kafkainput.Record{{Topic: "raw-flows", Partition: 1, Offset: 7}}
	if diff := helpers.Diff(commits, expected); diff != "" {
		t.Errorf("committed records (-got, +want):\n%s", diff)
	}
}

// TestTrackAfterDecode checks a record is only tracked once all its flows have
// been decoded: a flush happening in the middle of Decode (a full batch) must
// not commit the record while the rest of its flows are still in memory.
func TestTrackAfterDecode(t *testing.T) {
	clickhouseComponent := &eagerClickHouse{}
	c := newCoreForCommitTest(t, clickhouseComponent)

	var commits []kafkainput.Record
	receiveFunc, shutdown, _ := c.newWorker(0, make(chan kafkainput.ScaleRequest, 10),
		func(record kafkainput.Record) {
			commits = append(commits, record)
		})
	defer shutdown()

	record := makeRecord(t)
	if err := receiveFunc(t.Context(), record); err != nil {
		t.Fatalf("receiveFunc() error:\n%+v", err)
	}

	// The worker flushed during Decode, before the record was tracked.
	if len(commits) != 0 {
		t.Fatalf("record committed before decoding completed: %+v", commits)
	}

	// The next flush commits it.
	clickhouseComponent.worker().Flush(t.Context())
	expected := []kafkainput.Record{{Topic: "raw-flows", Partition: 3, Offset: 42}}
	if diff := helpers.Diff(commits, expected); diff != "" {
		t.Errorf("committed records (-got, +want):\n%s", diff)
	}
}
