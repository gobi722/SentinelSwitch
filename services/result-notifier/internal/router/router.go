// Package router resolves a FraudResultEvent's client_id to the external
// Kafka topic it should be republished to.
package router

import (
	"fmt"
	"regexp"
)

// clientIDPattern constrains client_id to Kafka's legal topic-name charset,
// since it is interpolated directly into a topic name below. Also guards
// against empty/garbage client_id values reaching kafka-go's writer.
var clientIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,200}$`)

// Router turns a client_id into its dedicated external results topic name.
type Router struct {
	topicPrefix string
}

// New constructs a Router. topicPrefix is prepended to client_id, e.g.
// "results." -> "results.<client_id>".
func New(topicPrefix string) *Router {
	return &Router{topicPrefix: topicPrefix}
}

// ResolveTopic returns the topic a given client_id's results should be
// published to. Returns an error if client_id is empty or contains
// characters unsafe for a Kafka topic name — callers should route such
// events to the unrouted DLQ rather than publish blindly.
func (r *Router) ResolveTopic(clientID string) (string, error) {
	if !clientIDPattern.MatchString(clientID) {
		return "", fmt.Errorf("router: invalid or missing client_id %q", clientID)
	}
	return r.topicPrefix + clientID, nil
}
