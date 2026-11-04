package crewcli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
)

// ServeArgs reads a driver's command line once its own commands are handled.
// Only `serve` or flags (older units have no `serve`) run the driver; anything
// else gets the usage text, because the driver already runs as a user unit and
// a bare command from a session must not start a second one. ok is false when
// the process should exit with code.
func ServeArgs(args []string, usage string, stdout, stderr io.Writer) (flags []string, code int, ok bool) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return nil, 2, false
	}
	switch first := args[0]; {
	case first == "help" || first == "-h" || first == "-help" || first == "--help":
		fmt.Fprint(stdout, usage)
		return nil, 0, false
	case first == "serve":
		return args[1:], 0, true
	case strings.HasPrefix(first, "-"):
		return args, 0, true
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", first, usage)
		return nil, 2, false
	}
}

// Listen binds a driver's port. A driver binds before it touches the fabric,
// so a second copy stops here and never claims a delivery the running one
// should have.
func Listen(addr string) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf("already running on %s; it runs as a user unit, so there is no need to start it", addr)
	}
	return listener, err
}
