module github.com/sentinelswitch/risk-service

go 1.25.0

require (
	github.com/grpc-ecosystem/go-grpc-middleware v1.4.0
	github.com/prometheus/client_golang v1.19.0
	github.com/sentinelswitch/proto v0.0.0
	go.uber.org/zap v1.27.0
	google.golang.org/grpc v1.83.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/prometheus/client_model v0.6.0 // indirect
	github.com/prometheus/common v0.51.1 // indirect
	github.com/prometheus/procfs v0.13.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/sentinelswitch/proto => ../proto-gen
