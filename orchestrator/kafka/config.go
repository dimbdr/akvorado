// SPDX-FileCopyrightText: 2022 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package kafka

import (
	"akvorado/common/kafka"
)

// InputConfiguration describes the configuration for the Kafka configurator.
type InputConfiguration struct {
	kafka.Configuration `mapstructure:",squash" yaml:",inline"`
	// ManageTopic tells if the input Kafka topic should be managed (create/update). Default is true.
	ManageTopic bool
	// TopicConfiguration describes the input topic configuration.
	TopicConfiguration TopicConfiguration
}

// OutputConfiguration describes an output Kafka topic for the orchestrator to
// manage — currently the outlet's kafka-output topic. It is a peer of the input
// Kafka configuration, with its own connection (brokers/TLS/SASL), so the
// output topic can live on a different cluster than the input topic. It is
// managed whenever it is configured (presence is the opt-in; no separate
// toggle), independently of ManageTopic. The schema hash is appended to its
// topic name, matching what kafka-output produces.
type OutputConfiguration struct {
	kafka.Configuration `mapstructure:",squash" yaml:",inline"`
	// TopicConfiguration is the partitions/replication/retention for the topic.
	TopicConfiguration `mapstructure:",squash" yaml:",inline"`
}

// TemplateConfiguration describes the compacted Kafka topic shared by the
// outlets to exchange NetFlow/IPFIX templates and sampling rates.
type TemplateConfiguration struct {
	kafka.Configuration `mapstructure:",squash" yaml:",inline"`
	// Enabled tells if the shared template cache is enabled for the outlets.
	Enabled bool
	// ManageTopic tells if the template Kafka topic should be managed
	// (create/update).
	ManageTopic bool
	// TopicConfiguration describes the template topic configuration.
	TopicConfiguration TopicConfiguration
}

// strptr returns a pointer to the provided string, for topic config entries.
func strptr(s string) *string {
	return &s
}

// TopicConfiguration describes the configuration for a topic
type TopicConfiguration struct {
	// NumPartitions tells how many partitions should be used for the topic.
	NumPartitions int32 `validate:"min=1"`
	// ReplicationFactor tells the replication factor for the topic.
	ReplicationFactor int16 `validate:"min=1"`
	// ConfigEntries is a map to specify the topic overrides. Non-listed overrides will be removed by default.
	ConfigEntries map[string]*string
	// ConfigEntriesStrictSync says if non-listed overrides should be removed (strict sync) or not. Default is True.
	ConfigEntriesStrictSync bool
}

// DefaultInputConfiguration represents the default input configuration for the Kafka configurator.
func DefaultInputConfiguration() InputConfiguration {
	return InputConfiguration{
		Configuration: kafka.DefaultConfiguration(),
		ManageTopic:   true,
		TopicConfiguration: TopicConfiguration{
			NumPartitions:           1,
			ReplicationFactor:       1,
			ConfigEntriesStrictSync: true,
		},
	}
}

// DefaultTemplateConfiguration represents the default configuration for the
// shared template topic.
//
// The topic is compacted: `cleanup.policy=compact,delete` with `retention.ms=-1`
// disables time-based deletion, so only tombstones are expired (`delete`) and
// compaction still deduplicates. A rarely re-sent options template is therefore
// never deleted; losing one costs a startup burst of undecodable flows.
func DefaultTemplateConfiguration() TemplateConfiguration {
	return TemplateConfiguration{
		Configuration: kafka.Configuration{
			Topic:   "templates",
			Brokers: []string{"127.0.0.1:9092"},
		},
		Enabled:     false,
		ManageTopic: false,
		TopicConfiguration: TopicConfiguration{
			NumPartitions:     1,
			ReplicationFactor: 1,
			ConfigEntries: map[string]*string{
				"cleanup.policy":            strptr("compact,delete"),
				"message.timestamp.type":    strptr("CreateTime"),
				"segment.ms":                strptr("600000"),
				"min.cleanable.dirty.ratio": strptr("0.1"),
				"delete.retention.ms":       strptr("86400000"),
				"retention.ms":              strptr("-1"),
			},
			ConfigEntriesStrictSync: true,
		},
	}
}

// ShouldAlterConfiguration validates if topic configuration update is needed regarding in-sync policy.
func ShouldAlterConfiguration(target, source map[string]*string, strict bool) bool {
	for k, v := range target {
		if ov, ok := source[k]; !ok || *ov != *v {
			return true
		}
	}
	if !strict {
		return false
	}
	return len(target) != len(source)
}
