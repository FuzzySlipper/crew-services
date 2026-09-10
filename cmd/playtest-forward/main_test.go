package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

func TestServeForwardsBidirectionallyAcrossHalfClose(t *testing.T) {
	upstream := mustListen(t)
	defer upstream.Close()

	upstreamDone := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			upstreamDone <- err
			return
		}
		defer conn.Close()
		request, err := io.ReadAll(conn)
		if err != nil {
			upstreamDone <- err
			return
		}
		if string(request) != "request" {
			upstreamDone <- errors.New("unexpected request: " + string(request))
			return
		}
		_, err = conn.Write([]byte("response"))
		upstreamDone <- err
	}()

	client, accepted := connectedPair(t)
	defer client.Close()
	var logs bytes.Buffer
	f := forwarder{dial: net.DialTimeout, log: log.New(&logs, "", 0)}
	forwardDone := make(chan struct{})
	go func() {
		f.serve(accepted, upstream.Addr().String())
		close(forwardDone)
	}()

	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != "response" {
		t.Fatalf("response = %q, want response", response)
	}
	if err := <-upstreamDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-forwardDone:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not finish")
	}
	if logs.Len() != 0 {
		t.Fatalf("normal connection logged diagnostics: %s", logs.String())
	}
}

func TestServeLogsDialFailure(t *testing.T) {
	client, accepted := net.Pipe()
	defer client.Close()
	var logs bytes.Buffer
	f := forwarder{
		dial: func(string, string, time.Duration) (net.Conn, error) {
			return nil, errors.New("upstream unavailable")
		},
		log: log.New(&logs, "", 0),
	}

	f.serve(accepted, "127.0.0.1:37301")

	got := logs.String()
	for _, want := range []string{"connection_id=1", "event=dial_failure", `upstream="127.0.0.1:37301"`, "duration=", "upstream unavailable"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic %q missing from %q", want, got)
		}
	}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func connectedPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener := mustListen(t)
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	accepted, err := listener.Accept()
	listener.Close()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	return client, accepted
}

type failedReadConn struct{ net.Conn }

func (c failedReadConn) Read([]byte) (int, error) { return 0, errors.New("upstream reset probe") }

func TestCopyFailureReportsOriginalDirectionAndSuppressesCleanupError(t *testing.T) {
	client, accepted := net.Pipe()
	defer client.Close()
	remote, peer := net.Pipe()
	defer peer.Close()
	var logs bytes.Buffer
	f := forwarder{
		dial: func(string, string, time.Duration) (net.Conn, error) { return failedReadConn{remote}, nil },
		log:  log.New(&logs, "", 0),
	}
	f.serve(accepted, "test-upstream")
	got := logs.String()
	for _, want := range []string{"event=copy_failure", "direction=upstream_to_client", "upstream_local=", "client_to_upstream_bytes=0", "upstream_to_client_bytes=0", "upstream reset probe"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Count(got, "event=") != 1 {
		t.Fatalf("cleanup produced extra failure: %s", got)
	}
}
