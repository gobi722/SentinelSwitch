module live-dashboard

go 1.25.0

replace github.com/sentinelswitch/proto => ../../../services/proto-gen

require (
	github.com/segmentio/kafka-go v0.4.47
	github.com/sentinelswitch/proto v0.0.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/klauspost/compress v1.15.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.1.2 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	golang.org/x/text v0.37.0 // indirect
)
