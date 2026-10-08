package component

import "syscall"

func setParentDeathSignal(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
