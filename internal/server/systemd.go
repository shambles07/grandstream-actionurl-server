package server

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// sdListenFDsStart is the first file descriptor systemd passes (SD_LISTEN_FDS_START).
const sdListenFDsStart = 3

// SystemdListener returns the listening socket passed by systemd socket
// activation (a .socket unit), or nil if the process was not socket
// activated. It implements the LISTEN_PID/LISTEN_FDS protocol from
// sd_listen_fds(3) without linking libsystemd, and uses the first socket if
// several are passed.
func SystemdListener() (net.Listener, error) {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n < 1 {
		return nil, nil
	}
	// Don't let child processes think the sockets are meant for them.
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_FDNAMES")

	f := os.NewFile(sdListenFDsStart, "systemd-socket")
	defer f.Close() // net.FileListener dups the descriptor
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("systemd socket: %w", err)
	}
	return ln, nil
}
