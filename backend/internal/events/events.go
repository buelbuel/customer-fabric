// Package events defines the versioned envelope Customer Fabric publishes from its PostgreSQL outbox and
// the Kafka transport that carries it (ADR 0013). Envelopes hold opaque references only, never profile
// attributes or other customer values.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// Schema names the envelope version; a consumer refuses envelopes it does not know.
const Schema = "customer-fabric.event.v1"

// Event is one committed profile change. ID is the mutation ID, so it is stable when the relay
// republishes after a crash and consumers deduplicate by it.
type Event struct {
	Schema         string    `json:"schema"`
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	Tenant         string    `json:"tenant"`
	Source         string    `json:"source"`
	ProfileID      string    `json:"profile_id"`
	ProfileVersion int64     `json:"profile_version"`
	OccurredAt     time.Time `json:"occurred_at"`
}

var (
	tenantName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	topicName  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,249}$`)
)

// ValidTenant reports whether name can identify a tenant in envelopes and topic names.
func ValidTenant(name string) bool { return tenantName.MatchString(name) }

// Kafka publishes events to one topic, keyed by profile so each profile's events stay in order on
// one partition. The idempotent producer retries without reordering or duplicating within a session.
type Kafka struct {
	client *kgo.Client
	topic  string
}

func NewKafka(brokers []string, topic string) (*Kafka, error) {
	if len(brokers) == 0 || !topicName.MatchString(topic) {
		return nil, errors.New("Kafka needs brokers and a valid topic name")
	}
	client, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.DefaultProduceTopic(topic),
		kgo.RequiredAcks(kgo.AllISRAcks()), kgo.DialTimeout(5*time.Second), kgo.RecordDeliveryTimeout(30*time.Second))
	if err != nil {
		return nil, err
	}
	return &Kafka{client: client, topic: topic}, nil
}

func (k *Kafka) Close() { k.client.Close() }

// EnsureTopic creates the topic with bounded retention unless it already exists. An existing topic's
// settings are left to the operator.
func (k *Kafka) EnsureTopic(ctx context.Context, partitions int32, retention time.Duration) error {
	topic := kmsg.NewCreateTopicsRequestTopic()
	topic.Topic, topic.NumPartitions, topic.ReplicationFactor = k.topic, partitions, -1
	for name, value := range map[string]string{"retention.ms": strconv.FormatInt(retention.Milliseconds(), 10), "cleanup.policy": "delete"} {
		config := kmsg.NewCreateTopicsRequestTopicConfig()
		config.Name, config.Value = name, &value
		topic.Configs = append(topic.Configs, config)
	}
	req := kmsg.NewPtrCreateTopicsRequest()
	req.Topics, req.TimeoutMillis = []kmsg.CreateTopicsRequestTopic{topic}, 10000
	res, err := req.RequestWith(ctx, k.client)
	if err != nil {
		return err
	}
	for _, t := range res.Topics {
		if err := kerr.ErrorForCode(t.ErrorCode); err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
			return err
		}
	}
	return nil
}

// Encode makes the Kafka record for an event: keyed by profile, so a profile's events share a partition.
func Encode(e Event) (*kgo.Record, error) {
	value, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return &kgo.Record{Key: []byte(e.ProfileID), Value: value}, nil
}

// Publish returns only after the brokers acknowledged every event, or the first failure.
func (k *Kafka) Publish(ctx context.Context, events []Event) error {
	records := make([]*kgo.Record, 0, len(events))
	for _, e := range events {
		record, err := Encode(e)
		if err != nil {
			return err
		}
		records = append(records, record)
	}
	return k.client.ProduceSync(ctx, records...).FirstErr()
}
