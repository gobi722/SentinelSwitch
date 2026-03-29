package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration structure for the Risk Service.
type Config struct {
	Server             ServerConfig      `yaml:"server"`
	DecisionThresholds ThresholdConfig   `yaml:"decision_thresholds"`
	Scoring            ScoringConfig     `yaml:"scoring"`
	Performance        PerformanceConfig `yaml:"performance"`
	Logging            LoggingConfig     `yaml:"logging"`
}

type ServerConfig struct {
	GRPC    GRPCConfig    `yaml:"grpc"`
	Metrics MetricsConfig `yaml:"metrics"`
	Health  HealthConfig  `yaml:"health"`
}

type GRPCConfig struct {
	Host                string `yaml:"host"`
	Port                int    `yaml:"port"`
	EnableReflection    bool   `yaml:"enable_reflection"`
	MaxConcurrentStreams int    `yaml:"max_concurrent_streams"`
}

type MetricsConfig struct {
	Port int    `yaml:"port"`
	Path string `yaml:"path"`
}

type HealthConfig struct {
	Port int `yaml:"port"`
}

type ThresholdConfig struct {
	ApproveBelow int `yaml:"approve_below"`
	ReviewFrom   int `yaml:"review_from"`
	ReviewTo     int `yaml:"review_to"`
	DeclineAbove int `yaml:"decline_above"`
}

type ScoringConfig struct {
	Model                       string          `yaml:"model"`
	Features                    []FeatureConfig `yaml:"features"`
	ContributingFactorThreshold float64         `yaml:"contributing_factor_threshold"`
}

type FeatureConfig struct {
	Name      string          `yaml:"name"`
	Weight    float64         `yaml:"weight"`
	Normalise NormaliseConfig `yaml:"normalise"`
}

type NormaliseConfig struct {
	Method string  `yaml:"method"`
	Cap    float64 `yaml:"cap"`
}

type PerformanceConfig struct {
	ScoringWorkers   int `yaml:"scoring_workers"`
	RequestTimeoutMs int `yaml:"request_timeout_ms"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// envVarRe matches ${VAR:-default} patterns.
var envVarRe = regexp.MustCompile(`\$\{([^}:]+)(?::-([^}]*))?\}`)

// expandEnv replaces ${VAR:-default} tokens with environment variable values.
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

	cfg := &Config{
		Server: ServerConfig{
			GRPC: GRPCConfig{
				Host:                "0.0.0.0",
				Port:                50052,
				MaxConcurrentStreams: 500,
			},
			Metrics: MetricsConfig{Port: 9094, Path: "/metrics"},
			Health:  HealthConfig{Port: 8084},
		},
		DecisionThresholds: ThresholdConfig{
			ApproveBelow: 400,
			ReviewFrom:   400,
			ReviewTo:     700,
			DeclineAbove: 700,
		},
		Scoring: ScoringConfig{
			ContributingFactorThreshold: 0.05,
		},
		Performance: PerformanceConfig{RequestTimeoutMs: 450},
		Logging:     LoggingConfig{Level: "info", Format: "json"},
	}

	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal %q: %w", path, err)
	}

	if cfg.Server.GRPC.Port == 0 {
		return nil, fmt.Errorf("config: server.grpc.port must be set")
	}

	return cfg, nil
}
