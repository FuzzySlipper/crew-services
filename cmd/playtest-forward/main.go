// playtest-forward runs inside the browser container to expose the game as localhost.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

type forwarder struct {
	dial  func(network, address string, timeout time.Duration) (net.Conn, error)
	log   *log.Logger
	trace bool
	next  atomic.Uint64
}

type copyResult struct {
	direction string
	bytes     int64
	err       error
}

func main() {
	port := flag.Int("port", 37301, "container loopback port")
	host := flag.String("host", "", "game server host")
	target := flag.Int("target-port", 37301, "game server port")
	trace := flag.Bool("trace", false, "log successful connection lifecycle events")
	flag.Parse()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		log.Fatal(err)
	}
	forwarder := forwarder{
		dial:  net.DialTimeout,
		log:   log.Default(),
		trace: *trace,
	}
	for {
		client, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go forwarder.serve(client, net.JoinHostPort(*host, fmt.Sprint(*target)))
	}
}

func (f *forwarder) serve(client net.Conn, upstream string) {
	connectionID := f.next.Add(1)
	peer := client.RemoteAddr().String()
	started := time.Now()

	remote, err := f.dial("tcp", upstream, 10*time.Second)
	if err != nil {
		f.log.Printf("playtest-forward connection_id=%d event=dial_failure peer=%q upstream=%q duration=%s error=%v", connectionID, peer, upstream, time.Since(started), err)
		_ = client.Close()
		return
	}
	defer client.Close()
	defer remote.Close()
	upstreamLocal := remote.LocalAddr().String()

	if f.trace {
		f.log.Printf("playtest-forward connection_id=%d event=connected peer=%q upstream=%q upstream_local=%q", connectionID, peer, upstream, upstreamLocal)
	}

	results := make(chan copyResult, 1)
	go func() {
		count, err := io.Copy(remote, client)
		if tcp, ok := remote.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		results <- copyResult{direction: "client_to_upstream", bytes: count, err: err}
	}()
	count, err := io.Copy(client, remote)
	upstreamResult := copyResult{direction: "upstream_to_client", bytes: count, err: err}
	// Preserve the existing forwarding lifecycle: upstream EOF closes both
	// sockets and unblocks the client copier. Do not report our own close as
	// another transport fault.
	_ = client.Close()
	_ = remote.Close()
	clientResult := <-results
	if upstreamResult.err != nil {
		f.logCopyFailure(connectionID, peer, upstream, upstreamLocal, started, upstreamResult, clientResult)
	} else if clientResult.err != nil && !errors.Is(clientResult.err, net.ErrClosed) {
		f.logCopyFailure(connectionID, peer, upstream, upstreamLocal, started, clientResult, upstreamResult)
	} else if f.trace {
		f.log.Printf("playtest-forward connection_id=%d event=eof peer=%q upstream=%q upstream_local=%q duration=%s client_to_upstream_bytes=%d upstream_to_client_bytes=%d", connectionID, peer, upstream, upstreamLocal, time.Since(started), clientResult.bytes, upstreamResult.bytes)
	}
}

func (f *forwarder) logCopyFailure(connectionID uint64, peer, upstream, upstreamLocal string, started time.Time, failed, other copyResult) {
	f.log.Printf("playtest-forward connection_id=%d event=copy_failure direction=%s peer=%q upstream=%q upstream_local=%q duration=%s client_to_upstream_bytes=%d upstream_to_client_bytes=%d error=%v", connectionID, failed.direction, peer, upstream, upstreamLocal, time.Since(started), bytesFor("client_to_upstream", failed, other), bytesFor("upstream_to_client", failed, other), failed.err)
}

func bytesFor(direction string, first, second copyResult) int64 {
	if first.direction == direction {
		return first.bytes
	}
	return second.bytes
}
