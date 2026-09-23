package provisioning

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/sentinelswitch/api-gateway/internal/auth"
)

// ErrInvalidClientID is returned when client_id fails ClientIDPattern.
var ErrInvalidClientID = errors.New("provisioning: client_id must match ^[a-zA-Z0-9._-]{1,200}$")

// Result is returned after a successful Provision call. APIKey and
// SCRAMPassword are plaintext and unrecoverable afterwards — this service
// never stores either in plaintext (Postgres holds only a SHA-256 hash of
// the API key; the SCRAM password is only ever sent to Kafka as a derived
// SaltedPassword, never itself).
type Result struct {
	ClientID            string
	APIKey              string
	KafkaTopic          string
	SCRAMUsername       string
	SCRAMPassword       string
	ConsumerGroupPrefix string
}

// clientStore is the subset of *auth.Store this package depends on.
type clientStore interface {
	InsertClient(ctx context.Context, clientID, apiKeyHash, apiKeyPrefix, name string) error
}

// Service orchestrates onboarding a new external API client: the Postgres
// api_clients row, the Kafka SCRAM-SHA-256 credential, the dedicated
// results.<client_id> topic, and the ACLs restricting that credential to
// only that topic and a <client_id>.-prefixed consumer-group namespace.
// Mirrors scripts/provision-client.sh's four steps exactly, in the same
// order, so both paths produce an identical result.
type Service struct {
	clients clientStore
	kafka   *KafkaAdmin
	log     *zap.Logger
}

func NewService(clients clientStore, kafka *KafkaAdmin, log *zap.Logger) *Service {
	return &Service{clients: clients, kafka: kafka, log: log}
}

// Provision onboards clientID. Step order: Postgres row, then SCRAM
// credential, then topic, then ACLs — matching the script.
//
// This is not transactional across Postgres and Kafka (the script wasn't
// either): if a Kafka step fails after the Postgres row was already
// created, that row is NOT rolled back. Because client_id is the table's
// primary key, simply calling Provision again with the same client_id will
// fail at the Postgres step (ErrClientExists) rather than resuming the
// Kafka steps — the returned error says so explicitly, so an operator knows
// to either finish the Kafka side by hand (using client_id as the SCRAM/ACL
// principal, as scripts/provision-client.sh does) or delete the api_clients
// row and retry with a fresh client_id.
func (s *Service) Provision(ctx context.Context, clientID, displayName string) (*Result, error) {
	if !ClientIDPattern.MatchString(clientID) {
		return nil, ErrInvalidClientID
	}

	apiKey, err := newAPIKey()
	if err != nil {
		return nil, err
	}
	scramPassword, err := newSCRAMPassword()
	if err != nil {
		return nil, err
	}

	apiKeyHash := auth.Hash(apiKey)
	apiKeyPrefix := apiKey[:12]

	if err := s.clients.InsertClient(ctx, clientID, apiKeyHash, apiKeyPrefix, displayName); err != nil {
		if errors.Is(err, auth.ErrClientExists) {
			return nil, err
		}
		return nil, fmt.Errorf("provisioning: postgres insert: %w", err)
	}

	topic := "results." + clientID
	groupPrefix := clientID + "."

	if err := s.kafka.UpsertSCRAMCredential(ctx, clientID, scramPassword); err != nil {
		return nil, fmt.Errorf(
			"provisioning: api_clients row for %q was created (API key issued) but the Kafka SCRAM "+
				"credential step failed — complete it by hand (kafka-configs --alter --add-config "+
				"SCRAM-SHA-256=... --entity-name %s) or delete the api_clients row and retry: %w",
			clientID, clientID, err)
	}
	if err := s.kafka.CreateResultsTopic(ctx, topic); err != nil {
		return nil, fmt.Errorf(
			"provisioning: %q has a Postgres row and SCRAM credential but topic creation failed — "+
				"create %s by hand (kafka-topics --create) or delete the api_clients row and retry: %w",
			clientID, topic, err)
	}
	if err := s.kafka.GrantResultsACLs(ctx, clientID, topic, groupPrefix); err != nil {
		return nil, fmt.Errorf(
			"provisioning: %q has a Postgres row, SCRAM credential, and topic %s but ACL grant failed — "+
				"grant it by hand (kafka-acls --add) or delete the api_clients row and retry: %w",
			clientID, topic, err)
	}

	s.log.Info("client provisioned",
		zap.String("client_id", clientID),
		zap.String("display_name", displayName),
		zap.String("kafka_topic", topic),
	)

	return &Result{
		ClientID:            clientID,
		APIKey:              apiKey,
		KafkaTopic:          topic,
		SCRAMUsername:       clientID,
		SCRAMPassword:       scramPassword,
		ConsumerGroupPrefix: groupPrefix,
	}, nil
}
