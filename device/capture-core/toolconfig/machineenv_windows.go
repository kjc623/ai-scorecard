//go:build windows

package toolconfig

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// MachineEnvironment is the system environment in the registry.
func MachineEnvironment() MachineEnv { return registryEnv{} }

type registryEnv struct{}

func (registryEnv) Lookup(name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvKey, registry.QUERY_VALUE)
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// Set writes the value as the system's environment editor does: expandable when it names another
// variable.
func (registryEnv) Set(name, value string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if strings.Contains(value, "%") {
		err = k.SetExpandStringValue(name, value)
	} else {
		err = k.SetStringValue(name, value)
	}
	if err != nil {
		return err
	}
	announceEnvironment()
	return nil
}

func (registryEnv) Unset(name string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	announceEnvironment()
	return nil
}

// announceEnvironment broadcasts WM_SETTINGCHANGE for "Environment", as setx does, so top-level
// windows that listen reload the environment. The broadcast reaches only the windows of the
// sender's own session.
func announceEnvironment() { winRegistry{}.environmentChanged() }
