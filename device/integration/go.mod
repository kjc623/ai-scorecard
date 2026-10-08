module github.com/shadow-ai-capture/device/integration

go 1.27.0

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	github.com/shadow-ai-capture/contracts v0.0.0
	github.com/shadow-ai-capture/device/capture-core v0.0.0
	github.com/shadow-ai-capture/device/capture-spool v0.0.0
	github.com/shadow-ai-capture/device/classifier-host v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
	golang.org/x/sys v0.48.0
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/StackExchange/wmi v1.2.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/certtostore v1.0.7 // indirect
	github.com/google/deck v1.1.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.31.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.1 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/grpc v1.84.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/shadow-ai-capture/device/protocol => ../protocol

replace github.com/shadow-ai-capture/device/capture-core => ../capture-core

replace github.com/shadow-ai-capture/device/capture-spool => ../capture-spool

replace github.com/shadow-ai-capture/device/classifier-host => ../classifier-host

replace github.com/shadow-ai-capture/contracts => ../../contracts/generated/go
