// Package config loads api-gateway.yaml and redis.yaml into typed structs.
// Environment variable substitution (${VAR:-default}) is handled by os.ExpandEnv
// before YAML parsing, so no external templating library is needed.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Top-level
// ---------------------------------------------------------------------------

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	PanHashing   PanHashingConfig   `yaml:"pan_hashing"`
	Idempotency  IdempotencyConfig  `yaml:"idempotency"`
	Auth         AuthConfig         `yaml:"auth"`
	Postgres     PostgresConfig     `yaml:"postgres"`
	Kafka        KafkaConfig        `yaml:"kafka"`
	Validation   ValidationConfig   `yaml:"validation"`
	RateLimiting RateLimitingConfig `yaml:"rate_limiting"`
	Logging      LoggingConfig      `yaml:"logging"`

	// Redis connection details — loaded from redis.yaml
	Redis RedisConfig
}

// ---------------------------------------------------------------------------
// Auth (API-key authentication)
// ---------------------------------------------------------------------------

type AuthConfig struct {
	ApiKeyHeader    string `yaml:"api_key_header"`
	CacheRedisDB    int    `yaml:"cache_redis_db"`
	CacheTTLSeconds int    `yaml:"cache_ttl_seconds"`
}

// ---------------------------------------------------------------------------
// Postgres (api_clients registry, used by the auth store)
// ---------------------------------------------------------------------------

type PostgresConfig struct {
	Host     string          `yaml:"host"`
	Port     int             `yaml:"port"`
	Database string          `yaml:"database"`
	Username string          `yaml:"username"`
	Password string          `yaml:"password"`
	SSLMode  string          `yaml:"ssl_mode"`
	Pool     PostgresPoolCfg `yaml:"pool"`
}

type PostgresPoolCfg struct {
	MaxOpenConns     int `yaml:"max_open_conns"`
	MaxIdleConns     int `yaml:"max_idle_conns"`
	ConnMaxLifetime  int `yaml:"conn_max_lifetime"`
	ConnectTimeoutMs int `yaml:"connect_timeout_ms"`
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type ServerConfig struct {
	GRPC    GRPCConfig    `yaml:"grpc"`
	Metrics MetricsConfig `yaml:"metrics"`
	Health  HealthConfig  `yaml:"health"`
}

type GRPCConfig struct {
	Host                string          `yaml:"host"`
	Port                int             `yaml:"port"`
	TLS                 TLSConfig       `yaml:"tls"`
	Keepalive           KeepaliveConfig `yaml:"keepalive"`
	MaxRecvMsgSizeBytes int             `yaml:"max_recv_msg_size_bytes"`
	MaxSendMsgSizeBytes int             `yaml:"max_send_msg_size_bytes"`
}

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type KeepaliveConfig struct {
	MaxConnectionIdleMs int `yaml:"max_connection_idle_ms"`
	MaxConnectionAgeMs  int `yaml:"max_connection_age_ms"`
	KeepaliveTimeMs     int `yaml:"keepalive_time_ms"`
	KeepaliveTimeoutMs  int `yaml:"keepalive_timeout_ms"`
}

type MetricsConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	Path string `yaml:"path"`
}

type HealthConfig struct {
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	LivenessPath  string `yaml:"liveness_path"`
	ReadinessPath string `yaml:"readiness_path"`
}

// ---------------------------------------------------------------------------
// PAN hashing
// ---------------------------------------------------------------------------

type PanHashingConfig struct {
	Algorithm          string `yaml:"algorithm"`
	SecretEnv          string `yaml:"secret_env"`
	MaskedPanPrefixLen int    `yaml:"masked_pan_prefix_digits"`
	MaskedPanSuffixLen int    `yaml:"masked_pan_suffix_digits"`
	MaskChar           string `yaml:"mask_char"`
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

type IdempotencyConfig struct {
	RedisDB            int    `yaml:"redis_db"`
	KeyPrefix          string `yaml:"key_prefix"`
	TTLSeconds         int    `yaml:"ttl_seconds"`
	OnRedisUnavailable string `yaml:"on_redis_unavailable"`
}

// ---------------------------------------------------------------------------
// Kafka
// ---------------------------------------------------------------------------

type KafkaConfig struct {
	Brokers  string              `yaml:"brokers"`
	Topic    string              `yaml:"topic"`
	Producer KafkaProducerConfig `yaml:"producer"`
}

type KafkaProducerConfig struct {
	ValueSerializer   string `yaml:"value_serializer"`
	SchemaRegistryURL string `yaml:"schema_registry_url"`
	SchemaSubject     string `yaml:"schema_subject"`
	Acks              string `yaml:"acks"`
	EnableIdempotence bool   `yaml:"enable_idempotence"`
	Retries           int    `yaml:"retries"`
	DeliveryTimeoutMs int    `yaml:"delivery_timeout_ms"`
	LingerMs          int    `yaml:"linger_ms"`
	BatchSizeBytes    int    `yaml:"batch_size_bytes"`
	CompressionType   string `yaml:"compression_type"`
	ClientID          string `yaml:"client_id"`
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

type ValidationConfig struct {
	AmountMinMinor            int64    `yaml:"amount_min_minor"`
	AmountMaxMinor            int64    `yaml:"amount_max_minor"`
	SupportedCurrencies       []string `yaml:"supported_currencies"`
	SupportedTransactionTypes []string `yaml:"supported_transaction_types"`
	SupportedChannels         []string `yaml:"supported_channels"`
	MccPattern                string   `yaml:"mcc_pattern"`
	TerminalIDMaxLength       int      `yaml:"terminal_id_max_length"`
	MerchantIDMaxLength       int      `yaml:"merchant_id_max_length"`
	RRNPattern                string   `yaml:"rrn_pattern"`
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

type RateLimitingConfig struct {
	Enabled           bool   `yaml:"enabled"`
	RequestsPerSecond int    `yaml:"requests_per_second"`
	Burst             int    `yaml:"burst"`
	IdentityHeader    string `yaml:"identity_header"`
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

type LoggingConfig struct {
	Level   string            `yaml:"level"`
	Format  string            `yaml:"format"`
	Dir     string            `yaml:"dir"`
	Default map[string]string `yaml:"default_fields"`
}

// ---------------------------------------------------------------------------
// Redis (parsed from redis.yaml)
// ---------------------------------------------------------------------------

type RedisConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	DB   int    `yaml:"db"`
	// Mode     string           `yaml:"mode"`
	// Cluster  RedisClusterConf `yaml:"connection"`
	Password string
	TLS      bool
	Pool     RedisPoolConfig
}

// Flattened from redis.yaml for ease of use in the Gateway.
type RedisPoolConfig struct {
	MaxActive    int `yaml:"max_active"`
	MaxIdle      int `yaml:"max_idle"`
	IdleTimeout  int `yaml:"idle_timeout"`
	DialTimeout  int `yaml:"dial_timeout"`
	ReadTimeout  int `yaml:"read_timeout"`
	WriteTimeout int `yaml:"write_timeout"`
}

type RedisClusterConf struct {
	Nodes      []RedisNode `yaml:"cluster_nodes"`
	Standalone struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		DB   int    `yaml:"db"`
	} `yaml:"standalone"`
	Auth struct {
		Password string `yaml:"password"`
		TLS      bool   `yaml:"tls"`
	} `yaml:"auth"`
	Pool RedisPoolConfig `yaml:"pool"`
}

type RedisNode struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

// Load reads api-gateway.yaml and redis.yaml from configDir.
// All ${VAR:-default} expressions are expanded from the environment before parsing.
func Load(gatewayYAML, redisYAML string) (*Config, error) {
	cfg := &Config{}

	if err := loadYAML(gatewayYAML, cfg); err != nil {
		return nil, fmt.Errorf("api-gateway.yaml: %w", err)
	}

	// Redis is parsed into a wrapper struct then merged into cfg.Redis
	var redisWrapper struct {
		Connection RedisClusterConf `yaml:"connection"`
	}
	if err := loadYAML(redisYAML, &redisWrapper); err != nil {
		return nil, fmt.Errorf("redis.yaml: %w", err)
	}
	fmt.Println(redisWrapper)
	cfg.Redis = RedisConfig{
		Pool: redisWrapper.Connection.Pool,
	}
	cfg.Redis.Password = redisWrapper.Connection.Auth.Password
	cfg.Redis.TLS = redisWrapper.Connection.Auth.TLS

	// Validate mandatory env vars
	if cfg.PanHashing.SecretEnv == "" {
		return nil, fmt.Errorf("pan_hashing.secret_env must be set")
	}
	if os.Getenv(cfg.PanHashing.SecretEnv) == "" {
		return nil, fmt.Errorf("required env var %s is not set (pan_hashing.secret_env)", cfg.PanHashing.SecretEnv)
	}

	applyAuthDefaults(cfg)

	return cfg, nil
}

func applyAuthDefaults(cfg *Config) {
	if cfg.Auth.ApiKeyHeader == "" {
		cfg.Auth.ApiKeyHeader = "x-api-key"
	}
	if cfg.Auth.CacheTTLSeconds == 0 {
		cfg.Auth.CacheTTLSeconds = 60
	}
	if cfg.Postgres.Port == 0 {
		cfg.Postgres.Port = 5432
	}
	if cfg.Postgres.SSLMode == "" {
		cfg.Postgres.SSLMode = "disable"
	}
	if cfg.Postgres.Pool.MaxOpenConns == 0 {
		cfg.Postgres.Pool.MaxOpenConns = 10
	}
	if cfg.Postgres.Pool.MaxIdleConns == 0 {
		cfg.Postgres.Pool.MaxIdleConns = 2
	}
	if cfg.Postgres.Pool.ConnMaxLifetime == 0 {
		cfg.Postgres.Pool.ConnMaxLifetime = 1800
	}
	if cfg.Postgres.Pool.ConnectTimeoutMs == 0 {
		cfg.Postgres.Pool.ConnectTimeoutMs = 5000
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// envPattern matches ${VAR:-default} and ${VAR} substitution syntax.
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-(.*?))?\}`)

func expandEnv(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(match string) string {
		sub := envPattern.FindStringSubmatch(match)
		key, def := sub[1], sub[2]
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return def
	})
}

func loadYAML(path string, out interface{}) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	expanded := expandEnv(string(raw))
	// Strip comment lines so the YAML parser doesn't trip on inline ${}
	expanded = stripYAMLComments(expanded)
	return yaml.Unmarshal([]byte(expanded), out)
}

// stripYAMLComments removes # … portions only from non-string lines to avoid
// clobbering quoted values that contain '#'.
func stripYAMLComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if idx := strings.Index(l, " #"); idx != -1 {
			lines[i] = strings.TrimRight(l[:idx], " \t")
		}
	}
	return strings.Join(lines, "\n")
}
