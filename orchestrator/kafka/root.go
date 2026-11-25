// SPDX-FileCopyrightText: 2022 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

// Package kafka handles Kafka-related configuration for the orchestrator.
package kafka

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"akvorado/common/kafka"
	"akvorado/common/pb"
	"akvorado/common/reporter"
	"akvorado/common/schema"
)

// Component represents the Kafka configurator.
type Component struct {
	r              *reporter.Reporter
	d              Dependencies
	inputConfig    InputConfiguration
	outputConfig   *OutputConfiguration
	templateConfig TemplateConfiguration

	inputOpts     []kgo.Opt
	inputTopic    string
	outputOpts    []kgo.Opt
	outputTopic   string
	templateOpts  []kgo.Opt
	templateTopic string
}

// Dependencies are the dependencies for the Kafka component
type Dependencies struct {
	Schema *schema.Component
}

// New creates a new Kafka configurator.
func New(r *reporter.Reporter, input InputConfiguration, output *OutputConfiguration, templates TemplateConfiguration, dependencies Dependencies) (*Component, error) {
	if !input.ManageTopic && output == nil && !templates.ManageTopic {
		r.Info().Msg("Kafka topic management disabled, skipping Kafka initialization")
		return nil, nil
	}

	c := Component{
		r:              r,
		d:              dependencies,
		inputConfig:    input,
		outputConfig:   output,
		templateConfig: templates,
	}
	if input.ManageTopic {
		inputOpts, err := kafka.NewConfig(r, input.Configuration)
		if err != nil {
			return nil, err
		}
		c.inputOpts = inputOpts
		c.inputTopic = fmt.Sprintf("%s-v%d", input.Topic, pb.Version)
	}
	if output != nil {
		// The output topic uses its own connection (possibly a different cluster
		// than the input) and mirrors the outlet's kafka-output naming: base name
		// plus the schema hash, so an incompatible schema change lands on a new
		// topic.
		outputOpts, err := kafka.NewConfig(r, output.Configuration)
		if err != nil {
			return nil, err
		}
		c.outputOpts = outputOpts
		c.outputTopic = fmt.Sprintf("%s-%s", output.Topic, dependencies.Schema.ProtobufMessageHash())
	}
	if templates.ManageTopic {
		templateOpts, err := kafka.NewConfig(r, templates.Configuration)
		if err != nil {
			return nil, err
		}
		c.templateOpts = templateOpts
		c.templateTopic = fmt.Sprintf("%s-v%d", templates.Topic, pb.Version)
	}
	return &c, nil
}

// Start starts Kafka configuration.
func (c *Component) Start() error {
	if c == nil {
		return nil
	}
	c.r.Info().Msg("starting Kafka component")
	defer c.r.Info().Msg("Kafka component stopped")

	if c.inputConfig.ManageTopic {
		if err := c.manage(context.Background(), c.inputOpts, c.inputConfig.Brokers, c.inputTopic, c.inputConfig.TopicConfiguration); err != nil {
			return err
		}
	}
	if c.outputConfig != nil {
		if err := c.manage(context.Background(), c.outputOpts, c.outputConfig.Brokers, c.outputTopic, c.outputConfig.TopicConfiguration); err != nil {
			return err
		}
	}
	if c.templateConfig.ManageTopic {
		// A template-topic failure must not stop the orchestrator: the outlets
		// keep decoding, with an empty shared cache, until the topic exists.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.manage(ctx, c.templateOpts, c.templateConfig.Brokers, c.templateTopic, c.templateConfig.TopicConfiguration); err != nil {
			c.r.Warn().Err(err).Msg("unable to manage template topic, continuing; the outlet cache will be empty until the topic exists")
		}
	}
	return nil
}

// manage connects to a cluster with the given client options and creates or
// reconciles the topic there. brokers is used only for log context.
func (c *Component) manage(ctx context.Context, opts []kgo.Opt, brokers []string, topic string, tc TopicConfiguration) error {
	client, err := kgo.NewClient(opts...)
	if err != nil {
		c.r.Err(err).
			Str("brokers", strings.Join(brokers, ",")).
			Msg("unable to create Kafka client for topic creation")
		return fmt.Errorf("unable to create Kafka client for topic creation: %w", err)
	}
	defer client.Close()
	admin := kadm.NewClient(client)
	topics, err := admin.ListTopics(ctx)
	if err != nil {
		c.r.Err(err).
			Str("brokers", strings.Join(brokers, ",")).
			Msg("unable to get metadata for topics")
		return fmt.Errorf("unable to get metadata for topics: %w", err)
	}
	return c.reconcileTopic(ctx, admin, topics, brokers, topic, tc)
}

// reconcileTopic creates the topic if it is missing, or aligns its partition
// count and configuration if it already exists.
func (c *Component) reconcileTopic(ctx context.Context, admin *kadm.Client, topics kadm.TopicDetails, brokers []string, topic string, tc TopicConfiguration) error {
	l := c.r.With().
		Str("brokers", strings.Join(brokers, ",")).
		Str("topic", topic).
		Logger()
	td, ok := topics[topic]
	if !ok {
		if _, err := admin.CreateTopics(ctx, tc.NumPartitions, tc.ReplicationFactor, tc.ConfigEntries, topic); err != nil {
			l.Err(err).Msg("unable to create topic")
			return fmt.Errorf("unable to create topic %q: %w", topic, err)
		}
		l.Info().Msg("topic created")
		return nil
	}
	nbPartitions := len(td.Partitions)
	if nbPartitions > int(tc.NumPartitions) {
		l.Warn().Msgf("cannot decrease the number of partitions (from %d to %d)",
			nbPartitions, tc.NumPartitions)
	} else if nbPartitions < int(tc.NumPartitions) {
		add := int(tc.NumPartitions) - nbPartitions
		if _, err := admin.CreatePartitions(ctx, add, topic); err != nil {
			l.Err(err).Msg("unable to add more partitions")
			return fmt.Errorf("unable to add more partitions to topic %q: %w", topic, err)
		}
		l.Info().Msg("number of partitions increased")
	}
	if int(tc.ReplicationFactor) != td.Partitions.NumReplicas() {
		// TODO: https://github.com/deviceinsight/kafkactl/blob/main/internal/topic/topic-operation.go
		l.Warn().Msgf("mismatch for replication factor: got %d, want %d",
			td.Partitions.NumReplicas(), tc.ReplicationFactor)
	}
	configs, err := admin.DescribeTopicConfigs(ctx, topic)
	if err != nil || len(configs) != 1 {
		l.Err(err).Msg("unable to get topic configuration")
		return fmt.Errorf("unable to get topic %q configuration: %w", topic, err)
	}
	got := map[string]*string{}
	for _, config := range configs[0].Configs {
		if config.Source == kmsg.ConfigSourceDynamicTopicConfig {
			got[config.Key] = config.Value
		}
	}
	if ShouldAlterConfiguration(tc.ConfigEntries, got, tc.ConfigEntriesStrictSync) {
		alterConfigs := []kadm.AlterConfig{}
		for k, v := range tc.ConfigEntries {
			alterConfigs = append(alterConfigs, kadm.AlterConfig{
				Op:    kadm.SetConfig,
				Name:  k,
				Value: v,
			})
		}
		for k, v := range got {
			if _, ok := tc.ConfigEntries[k]; !ok {
				alterConfigs = append(alterConfigs, kadm.AlterConfig{
					Op:    kadm.DeleteConfig,
					Name:  k,
					Value: v,
				})
			}
		}
		if _, err := admin.AlterTopicConfigs(ctx, alterConfigs, topic); err != nil {
			l.Err(err).Msg("unable to set topic configuration")
			return fmt.Errorf("unable to set topic configuration for %q: %w", topic, err)
		}
		l.Info().Msg("topic updated")
	}
	return nil
}
