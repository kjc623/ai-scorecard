package toolconfig

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// OllamaFingerprint is the loopback port entry Ollama's relocation serves.
const OllamaFingerprint = "app:ollama"

// ollamaHostVar is the variable Ollama's server takes its listen address from, and its clients the
// server's address.
const ollamaHostVar = "OLLAMA_HOST"

// MachineEnv is the machine-wide environment: the variables a process started afterwards inherits,
// unless the user's own environment sets the same name.
type MachineEnv interface {
	// Lookup returns a variable's value, and false when it is not set.
	Lookup(name string) (value string, ok bool, err error)
	Set(name, value string) error
	Unset(name string) error
}

// Ollama moves Ollama's server off its well-known port through the machine-wide OLLAMA_HOST, so the
// loopback broker can hold that port. Ollama reads the variable when it starts, so the move takes
// effect the next time Ollama starts.
type Ollama struct {
	env    MachineEnv
	backup backup
	mu     sync.Mutex
}

// NewOllama returns Ollama's relocation over env, with its backup in dir.
func NewOllama(dir state.Dir, env MachineEnv) *Ollama {
	return &Ollama{env: env, backup: backupFor(dir, "ollama")}
}

// Relocate records OLLAMA_HOST as it is, unless a record is already kept, and then points it at
// 127.0.0.1:upstreamPort.
func (o *Ollama) Relocate(upstreamPort int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if upstreamPort <= 0 || upstreamPort > 65535 {
		return fmt.Errorf("toolconfig: ollama: upstream port %d is not a TCP port", upstreamPort)
	}
	value, present, err := o.env.Lookup(ollamaHostVar)
	if err != nil {
		return fmt.Errorf("toolconfig: ollama: reading %s: %w", ollamaHostVar, err)
	}
	if err := o.backup.takeOnce(original{Present: present, Content: []byte(value)}); err != nil {
		return err
	}
	want := "127.0.0.1:" + strconv.Itoa(upstreamPort)
	if present && value == want {
		return nil
	}
	if err := o.env.Set(ollamaHostVar, want); err != nil {
		return fmt.Errorf("toolconfig: ollama: setting %s: %w", ollamaHostVar, err)
	}
	return nil
}

// Restore puts OLLAMA_HOST back as Relocate recorded it and deletes the record. Without a record it
// changes nothing.
func (o *Ollama) Restore() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	orig, ok, err := o.backup.load()
	if err != nil || !ok {
		return err
	}
	if orig.Present {
		err = o.env.Set(ollamaHostVar, string(orig.Content))
	} else {
		err = o.env.Unset(ollamaHostVar)
	}
	if err != nil {
		return fmt.Errorf("toolconfig: ollama: restoring %s: %w", ollamaHostVar, err)
	}
	return o.backup.drop()
}
