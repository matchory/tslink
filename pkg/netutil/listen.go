package netutil

import (
	"fmt"
	"net"
)

// ListenInNetNS opens a TCP listener on addr in the network namespace at
// nsPath. The socket belongs to the namespace it was created in, so it keeps
// listening there after the thread returns to the plugin's namespace, and
// only that namespace's processes reach a loopback address.
func ListenInNetNS(nsPath, addr string) (net.Listener, error) {
	var ln net.Listener
	err := inNetNS(nsPath, func() error {
		var err error
		ln, err = net.Listen("tcp", addr)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s in %s: %w", addr, nsPath, err)
	}
	return ln, nil
}
