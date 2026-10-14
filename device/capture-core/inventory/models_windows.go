//go:build windows

package inventory

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// SystemModelHost is the machine the service runs on, read only: the CLI scanner's profiles,
// environment and file versions, with the process list, process owners and TCP listeners.
func SystemModelHost() ModelHost { return winModelHost{} }

type winModelHost struct{ winCLIHost }

// UserEnv reads a value of the user's Environment key, unexpanded; a hive that is not loaded, or a
// missing key or value, is "".
func (winModelHost) UserEnv(sid, name string) (string, error) {
	path := sid + `\` + userEnvKey
	k, err := registry.OpenKey(registry.USERS, path, registry.QUERY_VALUE)
	if notFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s\\%s: %w", path, name, err)
	}
	return v, nil
}

// Processes lists the running processes.
func (winModelHost) Processes() ([]RunningProcess, error) {
	entries, err := hostinfo.Processes()
	if err != nil {
		return nil, err
	}
	out := make([]RunningProcess, 0, len(entries))
	for _, e := range entries {
		out = append(out, RunningProcess{PID: e.PID, Name: e.Name})
	}
	return out, nil
}

func (winModelHost) ProcessInfo(pid uint32) (hostinfo.Process, error) {
	return hostinfo.ProcessInfo(pid)
}

func (winModelHost) ListenersOn(port int) ([]uint32, error) { return hostinfo.ListenersOn(port) }
