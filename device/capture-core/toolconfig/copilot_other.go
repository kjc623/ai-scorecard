//go:build !windows

package toolconfig

import "errors"

// copilotSupported: VS Code's macOS and Linux policy locations and the CLI's environment there are
// not written yet, so the provider reports tool_version_unsupported here.
const copilotSupported = false

func systemMachineRegistry() machineRegistry { return noRegistry{} }

// noRegistry is the registry where there is none.
type noRegistry struct{}

var errNoRegistry = errors.New("toolconfig: this platform has no registry")

func (noRegistry) get(string, string) (regValue, bool, error) {
	return regValue{}, false, errNoRegistry
}
func (noRegistry) set(string, string, regValue) error             { return errNoRegistry }
func (noRegistry) remove(string, string) error                    { return errNoRegistry }
func (noRegistry) environmentChanged()                            {}
func (noRegistry) watch(_ string, stop <-chan struct{}, _ func()) { <-stop }
