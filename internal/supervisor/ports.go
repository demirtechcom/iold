package supervisor

import (
	"errors"
	"fmt"
	"net"
	"slices"
)

var ErrNoFreePort = errors.New("no free port in range")

// portScanWindow bounds how far above the preferred port Allocate looks.
const portScanWindow = 100

// PortFree reports whether the TCP port can currently be bound on all
// interfaces.
func PortFree(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

// AllocatePort returns the preferred port if it is bindable and not
// already reserved by another deployment, otherwise the next such port
// within the scan window. reserved is the set of ports recorded in
// local state, which may not be bound yet (e.g. a deployment that is
// still downloading).
func AllocatePort(preferred int, reserved []int) (int, error) {
	for port := preferred; port < preferred+portScanWindow; port++ {
		if slices.Contains(reserved, port) {
			continue
		}
		if PortFree(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("%w: %d-%d", ErrNoFreePort, preferred, preferred+portScanWindow-1)
}
