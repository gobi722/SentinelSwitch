package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// envVarRe matches ${VAR:-default} patterns.
var envVarRe = regexp.MustCompile(`\$\{([^}:]+)(?::-([^}]*))?\}`)

// Config is the root configuration structure for result-notifier.
type Config struct {
	Kafka   KafkaConfig   `yaml:"kafka"`
	Router  RouterConfig  `yaml:"router"`
	Retry   RetryConfig   `yaml:"retry"`
	Server  ServerConfig  `yaml:"server"`
	Logging LoggingConfig `yaml:"logging"`
}

type KafkaConfig struct {
	Brokers     []string          `yaml:"brokers"`
	Consumer    ConsumerConfig    `yaml:"consumer"`
	DLQProducer DLQProducerConfig `yaml:"dlq_producer"`
}

type ConsumerConfig struct {
	GroupID           string `yaml:"group_id"`
	Topic             string `yaml:"topic"`
	SchemaRegistryURL string `yaml:"schema_registry_url"`
	SchemaSubject     string `yaml:"schema_subject"`
	ClientID          string `yaml:"client_id"`
}

type DLQProducerConfig struct {
	Topic    string `yaml:"topic"`
	ClientID string `yaml:"client_id"`
}

// RouterConfig controls how a FraudResultEvent's client_id is turned into an
// external results topic name.
type RouterConfig struct {
	TopicPrefix string `yaml:"topic_prefix"` // e.g. "results." -> results.<client_id>
}

type RetryConfig struct {
	MaxAttempts    int     `yaml:"max_attempts"`
	InitialDelayMs int     `yaml:"initial_delay_ms"`
	Multiplier     float64 `yaml:"multiplier"`
	MaxDelayMs     int     `yaml:"max_delay_ms"`
}

type ServerConfig struct {
	Metrics PortConfig `yaml:"metrics"`
	Health  PortConfig `yaml:"health"`
}

type PortConfig struct {
	Port int `yaml:"port"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	File   string `yaml:"file"`
}

func expandEnv(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := envVarRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		varName := sub[1]
		defaultVal := ""
		if len(sub) >= 3 {
			defaultVal = sub[2]
		}
		if val, ok := os.LookupEnv(varName); ok && val != "" {
			return val
		}
		return defaultVal
	})
}

// Load reads and parses the YAML config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file %q: %w", path, err)
	}

	expanded := expandEnv(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal yaml: %w", err)
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Router.TopicPrefix == "" {
		cfg.Router.TopicPrefix = "results."
	}
	if cfg.Retry.MaxAttempts == 0 {
		cfg.Retry.MaxAttempts = 3
	}
	if cfg.Retry.InitialDelayMs == 0 {
		cfg.Retry.InitialDelayMs = 500
	}
	if cfg.Retry.Multiplier == 0 {
		cfg.Retry.Multiplier = 2.0
	}
	if cfg.Retry.MaxDelayMs == 0 {
		cfg.Retry.MaxDelayMs = 5000
	}
	if cfg.Server.Metrics.Port == 0 {
		cfg.Server.Metrics.Port = 9098
	}
	if cfg.Server.Health.Port == 0 {
		cfg.Server.Health.Port = 8085
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
}
