package provisioning

import (
	"context"
	"errors"
	"fmt"
	"net"

	kafkago "github.com/segmentio/kafka-go"
)

// KafkaAdmin performs the broker-side provisioning steps (SCRAM credential,
// topic, ACLs) against the trusted INTERNAL listener — the same one
// Persistence Service and Result Notifier already use to talk to Kafka, not
// the SASL PUBLIC listener external clients connect through. These are
// privileged admin operations; they never touch the PUBLIC listener.
type KafkaAdmin struct {
	client *kafkago.Client
	addr   net.Addr
}

// NewKafkaAdmin builds a KafkaAdmin targeting brokerAddr (e.g. "kafka:9093").
func NewKafkaAdmin(brokerAddr string) *KafkaAdmin {
	addr := kafkago.TCP(brokerAddr)
	return &KafkaAdmin{
		client: &kafkago.Client{Addr: addr},
		addr:   addr,
	}
}

// UpsertSCRAMCredential creates or replaces the SCRAM-SHA-256 credential for
// principal User:username. Only the derived SaltedPassword crosses the wire
// to the broker — never the plaintext password.
func (a *KafkaAdmin) UpsertSCRAMCredential(ctx context.Context, username, password string) error {
	salt, err := newSalt()
	if err != nil {
		return err
	}

	resp, err := a.client.AlterUserScramCredentials(ctx, &kafkago.AlterUserScramCredentialsRequest{
		Addr: a.addr,
		Upsertions: []kafkago.UserScramCredentialsUpsertion{
			{
				Name:           username,
				Mechanism:      kafkago.ScramMechanismSha256,
				Iterations:     scramIterations,
				Salt:           salt,
				SaltedPassword: saltedPassword(password, salt),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("provisioning: alter user scram credentials: %w", err)
	}
	for _, r := range resp.Results {
		if r.Error != nil {
			return fmt.Errorf("provisioning: scram credential upsert failed for %s: %w", r.User, r.Error)
		}
	}
	return nil
}

// CreateResultsTopic creates a single-partition, RF-1 topic (matches the
// single-broker local-dev cluster — see config/kafka-topics.yaml for the
// documented production divergence). Idempotent: an already-existing topic
// is not an error.
func (a *KafkaAdmin) CreateResultsTopic(ctx context.Context, topic string) error {
	resp, err := a.client.CreateTopics(ctx, &kafkago.CreateTopicsRequest{
		Addr: a.addr,
		Topics: []kafkago.TopicConfig{
			{
				Topic:             topic,
				NumPartitions:     1,
				ReplicationFactor: 1,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("provisioning: create topic %s: %w", topic, err)
	}
	if topicErr := resp.Errors[topic]; topicErr != nil && !errors.Is(topicErr, kafkago.TopicAlreadyExists) {
		return fmt.Errorf("provisioning: create topic %s: %w", topic, topicErr)
	}
	return nil
}

// GrantResultsACLs grants principal User:clientID Read access to topic and
// to any consumer group whose ID starts with groupPrefix — mirrors the two
// ACLs scripts/provision-client.sh granted via `kafka-acls`.
func (a *KafkaAdmin) GrantResultsACLs(ctx context.Context, clientID, topic, groupPrefix string) error {
	principal := "User:" + clientID

	resp, err := a.client.CreateACLs(ctx, &kafkago.CreateACLsRequest{
		Addr: a.addr,
		ACLs: []kafkago.ACLEntry{
			{
				ResourceType:        kafkago.ResourceTypeTopic,
				ResourceName:        topic,
				ResourcePatternType: kafkago.PatternTypeLiteral,
				Principal:           principal,
				Host:                "*",
				Operation:           kafkago.ACLOperationTypeRead,
				PermissionType:      kafkago.ACLPermissionTypeAllow,
			},
			{
				ResourceType:        kafkago.ResourceTypeGroup,
				ResourceName:        groupPrefix,
				ResourcePatternType: kafkago.PatternTypePrefixed,
				Principal:           principal,
				Host:                "*",
				Operation:           kafkago.ACLOperationTypeRead,
				PermissionType:      kafkago.ACLPermissionTypeAllow,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("provisioning: create acls: %w", err)
	}
	for _, e := range resp.Errors {
		if e != nil {
			return fmt.Errorf("provisioning: create acls: %w", e)
		}
	}
	return nil
}
