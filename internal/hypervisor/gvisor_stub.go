//go:build !linux

package hypervisor

import (
	"context"
	"fmt"
	"net"
)

// probeInSentryNetNS is a no-op on non-Linux platforms. The gVisor backend
// only runs on Linux, so this is never reachable in production.
func probeInSentryNetNS(_ context.Context, _ int, socketPath string) (net.Conn, error) {
	return nil, fmt.Errorf("netns dialing not supported on this platform (socket: %s)", socketPath)
}
