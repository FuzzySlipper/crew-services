package codexadapter

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func httptestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// fakeDaemon is a WebSocket JSON-RPC server on a Unix socket, standing in for
// the Codex daemon's control socket.
type fakeDaemon struct {
	path     string
	listener net.Listener
	// answer returns the result for one request, or nil for an error reply.
	answer func(method string, params json.RawMessage) any
	paths  chan string
}

func startFakeDaemon(t *testing.T, answer func(string, json.RawMessage) any) *fakeDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "codexd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	d := &fakeDaemon{path: path, listener: listener, answer: answer, paths: make(chan string, 4)}
	go d.serve()
	return d
}

func (d *fakeDaemon) serve() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		go d.handle(conn)
	}
}

func (d *fakeDaemon) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	d.paths <- request.URL.Path
	sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + websocketGUID))
	_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	server := &wsConn{conn: conn, reader: reader}
	// Server frames are unmasked; reuse the reader and write raw frames.
	write := func(opcode byte, fin bool, payload []byte) {
		head := []byte{opcode, 0}
		if fin {
			head[0] |= 0x80
		}
		if len(payload) < 126 {
			head[1] = byte(len(payload))
		} else {
			head[1] = 126
			head = binary.BigEndian.AppendUint16(head, uint16(len(payload)))
		}
		_, _ = conn.Write(append(head, payload...))
	}
	write(wsPing, true, []byte("hi"))
	_ = server.readMessages(func(message []byte) {
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(message, &call) != nil || len(call.ID) == 0 {
			return
		}
		if call.Method == "initialize" {
			note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "thread/status/changed", "params": map[string]any{"threadId": "t", "status": map[string]any{"type": "idle"}}})
			write(wsText, true, note)
		}
		var reply []byte
		if result := d.answer(call.Method, call.Params); result != nil {
			reply, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
		} else {
			reply, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32600, "message": "no active turn to steer"}})
		}
		// Send replies in two fragments to exercise reassembly.
		half := len(reply) / 2
		write(wsText, false, reply[:half])
		write(wsContinuation, true, reply[half:])
	})
}

func TestDialDaemonSpeaksJSONRPCOverTheControlSocket(t *testing.T) {
	daemon := startFakeDaemon(t, func(method string, params json.RawMessage) any {
		switch method {
		case "initialize":
			return map[string]any{"userAgent": "fake"}
		case "thread/loaded/list":
			return map[string]any{"data": []string{"t1", "t2"}}
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": "t1", "status": map[string]any{"type": "active", "activeFlags": []string{}}, "ephemeral": false}}
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}}}}
		case "turn/steer":
			return nil
		}
		return map[string]any{}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := DialDaemon(ctx, daemon.path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if path := <-daemon.paths; path != "/" {
		t.Fatalf("handshake path = %q", path)
	}
	notes := make(chan string, 4)
	client.SetNotificationHandler(func(method string, _ json.RawMessage) { notes <- method })
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case method := <-notes:
		if method != "thread/status/changed" {
			t.Fatalf("notification = %q", method)
		}
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered")
	}
	loaded, err := client.LoadedThreads(ctx)
	if err != nil || len(loaded) != 2 {
		t.Fatalf("loaded = %v, %v", loaded, err)
	}
	thread, err := client.ReadThread(ctx, "t1")
	if err != nil || thread.Status != "active" || !thread.AcceptsInput {
		t.Fatalf("thread = %+v, %v", thread, err)
	}
	turn, err := client.ActiveTurn(ctx, "t1")
	if err != nil || turn != "turn-1" {
		t.Fatalf("active turn = %q, %v", turn, err)
	}
	if err := client.Steer(ctx, "t1", "turn-1", "x", "c"); !IsRejection(err) {
		t.Fatalf("steer error = %v, want a rejection", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadedThreads(ctx); err == nil {
		t.Fatal("request succeeded after close")
	}
}

func TestDialDaemonFailsWhenNothingListens(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := DialDaemon(ctx, filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("dialed a socket that does not exist")
	}
}
