module github.com/shadow-ai-capture/device/capture-core

go 1.27

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/go-ole/go-ole v1.3.0
	github.com/godbus/dbus/v5 v5.2.2
	github.com/shadow-ai-capture/device/capture-spool v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
	golang.org/x/sys v0.48.0
	golang.org/x/text v0.42.0
)

replace github.com/shadow-ai-capture/device/protocol => ../protocol

replace github.com/shadow-ai-capture/device/capture-spool => ../capture-spool
