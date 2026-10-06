module github.com/shadow-ai-capture/device/integration

go 1.27.0

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	github.com/shadow-ai-capture/contracts v0.0.0
	github.com/shadow-ai-capture/device/capture-core v0.0.0
	github.com/shadow-ai-capture/device/capture-spool v0.0.0
	github.com/shadow-ai-capture/device/classifier-host v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
)

require (
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/shadow-ai-capture/device/protocol => ../protocol

replace github.com/shadow-ai-capture/device/capture-core => ../capture-core

replace github.com/shadow-ai-capture/device/capture-spool => ../capture-spool

replace github.com/shadow-ai-capture/device/classifier-host => ../classifier-host

replace github.com/shadow-ai-capture/contracts => ../../contracts/generated/go
