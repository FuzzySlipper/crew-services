package devserver

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

func healthURL(probeHost string, port int, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("http://%s:%d%s", probeHost, port, path)
}

func localURL(probeHost string, port int) string {
	return fmt.Sprintf("http://%s:%d/", probeHost, port)
}

func publicURL(publicHost string, port int) string {
	if strings.TrimSpace(publicHost) == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/", publicHost, port)
}

func portInUse(host string, port int, timeout time.Duration) bool {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func findFreePort(bindHost string, ports PortRange, blocked map[int]bool) (int, error) {
	for port := ports.Start; port <= ports.End; port++ {
		if blocked[port] {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		_ = listener.Close()
		return port, nil
	}
	return 0, fmt.Errorf("%w: %d-%d", ErrNoPortAvailable, ports.Start, ports.End)
}

func ResolvePublicHost(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" || configured == PublicHostAuto {
		return discoverLANHost()
	}
	return configured
}

func discoverLANHost() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		return ip.String()
	}
	return ""
}
