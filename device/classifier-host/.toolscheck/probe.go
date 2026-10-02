//go:build probe

package main

import (
	"fmt"
	"os/exec"

	"github.com/shadow-ai-capture/device/protocol"
)

func main() {
	fmt.Println(protocol.Version, exec.ErrNotFound)
}
