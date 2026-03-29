package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration structure for the fraud-engine service.
type Config struct {
	Kafka              KafkaConfig    `yaml:"kafka"`
	Redis              RedisConfig    `yaml:"redis"`
	Rules              RulesConfig    `yaml:"rules"`
	RiskService        RiskService    `yaml:"risk_service"`
	Concurrency        Concurrency    `yaml:"concurrency"`
	Server             ServerConfig   `yaml:"server"`
	Logging            LoggingConfig  `yaml:"logging"`
	DecisionThresholds DecisionThresh `yaml:"decision_thresholds"`
}

// ── Kafka ─────────────────────────────────────────────────────────────────────

type KafkaConfig struct {
	Brokers  []string      `yaml:"brokers"`
	Consumer KafkaConsumer `yaml:"consumer"`
	Producer KafkaProducer `yaml:"producer"`
}

type KafkaConsumer struct {
	GroupID           string `yaml:"group_id"`
	Topic             string `yaml:"topic"`
	SchemaRegistryURL string `yaml:"schema_registry_url"`
	ClientID          string `yaml:"client_id"`
}

type KafkaProducer struct {
	Topic             string `yaml:"topic"`
	SchemaRegistryURL string `yaml:"schema_registry_url"`
}

// ── Redis ─────────────────────────────────────────────────────────────────────

type RedisConfig struct {
	Host             string `yaml:"host"`
	Port             int    `yaml:"port"`
	Password         string `yaml:"password"`
	VelocityDB       int    `yaml:"velocity_db"`
	MerchantRiskDB   int    `yaml:"merchant_risk_db"`
	CircuitBreakerDB int    `yaml:"circuit_breaker_db"`
}

// ── Rules ─────────────────────────────────────────────────────────────────────

type RulesConfig struct {
	ConfigFile string `yaml:"config_file"`
}

// ── Risk service ──────────────────────────────────────────────────────────────

type RiskService struct {
	Host           string           `yaml:"host"`
	Port           int              `yaml:"port"`
	CallTimeoutMs  int              `yaml:"call_timeout_ms"`
	CircuitBreaker CircuitBreakerCfg `yaml:"circuit_breaker"`
}

type CircuitBreakerCfg struct {
	FailureThreshold   int    `yaml:"failure_threshold"`
	SuccessThreshold   int    `yaml:"success_threshold"`
	OpenDurationMs     int    `yaml:"open_duration_ms"`
	FallbackRiskScore  int32  `yaml:"fallback_risk_score"`
	StateKey           string `yaml:"state_key"`
	FailuresKey        string `yaml:"failures_key"`
	StateTTLSeconds    int    `yaml:"state_ttl_seconds"`
	FailuresTTLSeconds int    `yaml:"failures_ttl_seconds"`
}

// ── Concurrency ───────────────────────────────────────────────────────────────

type Concurrency struct {
	WorkerPoolSize    int `yaml:"worker_pool_size"`
	MaxConcurrentRisk int `yaml:"max_concurrent_risk_calls"`
}

// ── Server ────────────────────────────────────────────────────────────────────

type ServerConfig struct {
	Metrics PortConfig `yaml:"metrics"`
	Health  PortConfig `yaml:"health"`
}

type PortConfig struct {
	Port int `yaml:"port"`
}

// ── Logging ───────────────────────────────────────────────────────────────────

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// ── Decision thresholds ───────────────────────────────────────────────────────

type DecisionThresh struct {
	ApproveBelow int32 `yaml:"approve_below"`
	DeclineAbove int32 `yaml:"decline_above"`
}

// envVarRe matches ${VAR:-default} patterns.
var envVarRe = regexp.MustCompile(`\$\{([^}:]+)(?::-([^}]*))?\}`)

// expandEnv replaces ${VAR:-default} tokens in s with environment variable values.
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

// Load reads and parses the YAML config file at path, expanding environment
// variable references before unmarshalling.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}

	expanded := expandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Metrics.Port == 0 {
		cfg.Server.Metrics.Port = 9095
	}
	if cfg.Server.Health.Port == 0 {
		cfg.Server.Health.Port = 8082
	}
	if cfg.Concurrency.WorkerPoolSize == 0 {
		cfg.Concurrency.WorkerPoolSize = 100
	}
	if cfg.Concurrency.MaxConcurrentRisk == 0 {
		cfg.Concurrency.MaxConcurrentRisk = 50
	}
	if cfg.RiskService.CallTimeoutMs == 0 {
		cfg.RiskService.CallTimeoutMs = 500
	}
	if cfg.RiskService.Port == 0 {
		cfg.RiskService.Port = 50052
	}
	if cfg.RiskService.CircuitBreaker.FailureThreshold == 0 {
		cfg.RiskService.CircuitBreaker.FailureThreshold = 5
	}
	if cfg.RiskService.CircuitBreaker.SuccessThreshold == 0 {
		cfg.RiskService.CircuitBreaker.SuccessThreshold = 2
	}
	if cfg.RiskService.CircuitBreaker.OpenDurationMs == 0 {
		cfg.RiskService.CircuitBreaker.OpenDurationMs = 15000
	}
	if cfg.RiskService.CircuitBreaker.FallbackRiskScore == 0 {
		cfg.RiskService.CircuitBreaker.FallbackRiskScore = 600
	}
	if cfg.RiskService.CircuitBreaker.StateKey == "" {
		cfg.RiskService.CircuitBreaker.StateKey = "cb:risk_service:state"
	}
	if cfg.RiskService.CircuitBreaker.FailuresKey == "" {
		cfg.RiskService.CircuitBreaker.FailuresKey = "cb:risk_service:failures"
	}
	if cfg.RiskService.CircuitBreaker.StateTTLSeconds == 0 {
		cfg.RiskService.CircuitBreaker.StateTTLSeconds = 60
	}
	if cfg.RiskService.CircuitBreaker.FailuresTTLSeconds == 0 {
		cfg.RiskService.CircuitBreaker.FailuresTTLSeconds = 30
	}
	if cfg.DecisionThresholds.ApproveBelow == 0 {
		cfg.DecisionThresholds.ApproveBelow = 400
	}
	if cfg.DecisionThresholds.DeclineAbove == 0 {
		cfg.DecisionThresholds.DeclineAbove = 700
	}
	if cfg.Redis.Port == 0 {
		cfg.Redis.Port = 6379
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
}
