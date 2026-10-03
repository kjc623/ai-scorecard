module github.com/shadow-ai-capture/docs/risks/r1-harness

go 1.27

require (
	github.com/shadow-ai-capture/device/capture-core v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
)

require (
	github.com/shadow-ai-capture/device/canon v0.0.0 // indirect
	github.com/shadow-ai-capture/device/capture-spool v0.0.0 // indirect
)

replace github.com/shadow-ai-capture/device/capture-core => ../../../endpoint/capture-core

replace github.com/shadow-ai-capture/device/protocol => ../../../endpoint/protocol

replace github.com/shadow-ai-capture/device/canon => ../../../endpoint/canon

replace github.com/shadow-ai-capture/device/capture-spool => ../../../endpoint/capture-spool
