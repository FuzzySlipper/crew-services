package codexadapter

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultDaemonSocket is where the shared Codex app-server daemon (the one the
// TUI attaches to) listens: CODEX_HOME/app-server-control/app-server-control.sock.
func DefaultDaemonSocket() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, _ := os.UserHomeDir()
		home = filepath.Join(user, ".codex")
	}
	return filepath.Join(home, "app-server-control", "app-server-control.sock")
}

// DialDaemon connects to the running shared Codex daemon. The control socket
// speaks WebSocket (one JSON-RPC message per text frame), which is also what
// `codex app-server proxy` forwards byte for byte, so dialing it directly
// avoids a child process. It never starts, stops or restarts the daemon:
// when nothing listens, it fails.
func DialDaemon(ctx context.Context, socketPath string) (*StdioAppServer, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to Codex daemon: %w", err)
	}
	reader, err := websocketHandshake(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	prefix := make([]byte, 12)
	if _, err := rand.Read(prefix); err != nil {
		_ = conn.Close()
		return nil, err
	}
	ws := &wsConn{conn: conn, reader: reader}
	client := newAppServerClient(ws, hex.EncodeToString(prefix))
	go func() {
		err := ws.readMessages(client.handleFrame)
		if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			err = errors.New("Codex daemon connection closed")
		}
		client.fail(err)
	}()
	return client, nil
}

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// websocketHandshake upgrades conn with a client handshake on "/". The path
// matters: the daemon treats "/daemon/shutdown" as a shutdown request.
func websocketHandshake(ctx context.Context, conn net.Conn) (*bufio.Reader, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}
	defer conn.SetDeadline(time.Time{})
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	request := "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return nil, fmt.Errorf("write Codex daemon handshake: %w", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil, fmt.Errorf("read Codex daemon handshake: %w", err)
	}
	_ = response.Body.Close()
	sum := sha1.Sum([]byte(key + websocketGUID))
	if response.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") || response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("Codex daemon refused the WebSocket upgrade: %s", response.Status)
	}
	return reader, nil
}

// wsConn is a minimal RFC 6455 client: masked text frames out; text,
// continuation, ping and close frames in.
type wsConn struct {
	conn   net.Conn
	reader *bufio.Reader

	writeMu sync.Mutex
	closed  bool
}

const (
	wsContinuation = 0x0
	wsText         = 0x1
	wsBinary       = 0x2
	wsClose        = 0x8
	wsPing         = 0x9
	wsPong         = 0xA
	// maxWSMessage bounds one reassembled message; thread items can be large.
	maxWSMessage = 64 << 20
)

// Write sends p as one text message. Callers append a newline for the stdio
// framing; it is not part of the message.
func (w *wsConn) Write(p []byte) (int, error) {
	payload := p
	for len(payload) > 0 && payload[len(payload)-1] == '\n' {
		payload = payload[:len(payload)-1]
	}
	if err := w.writeFrame(wsText, payload); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *wsConn) Close() error {
	w.writeMu.Lock()
	already := w.closed
	w.writeMu.Unlock()
	if !already {
		_ = w.writeFrame(wsClose, []byte{0x03, 0xe8})
	}
	w.writeMu.Lock()
	w.closed = true
	w.writeMu.Unlock()
	return w.conn.Close()
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.closed {
		return net.ErrClosed
	}
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)
	frame := make([]byte, len(header)+len(payload))
	copy(frame, header)
	for i, b := range payload {
		frame[len(header)+i] = b ^ mask[i%4]
	}
	_, err := w.conn.Write(frame)
	return err
}

// readMessages delivers each complete text message to handle until the
// connection ends.
func (w *wsConn) readMessages(handle func([]byte)) error {
	var message []byte
	for {
		fin, opcode, payload, err := w.readFrame()
		if err != nil {
			return err
		}
		switch opcode {
		case wsPing:
			if err := w.writeFrame(wsPong, payload); err != nil {
				return err
			}
		case wsPong:
		case wsClose:
			return io.EOF
		case wsText, wsBinary, wsContinuation:
			if opcode != wsContinuation {
				message = message[:0]
			}
			if len(message)+len(payload) > maxWSMessage {
				return errors.New("Codex daemon message is too large")
			}
			message = append(message, payload...)
			if fin {
				if opcode != wsBinary {
					handle(message)
				}
				message = nil
			}
		default:
			return fmt.Errorf("unexpected WebSocket opcode %d", opcode)
		}
	}
}

func (w *wsConn) readFrame() (bool, byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(w.reader, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin, opcode := head[0]&0x80 != 0, head[0]&0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.reader, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.reader, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > maxWSMessage {
		return false, 0, nil, errors.New("Codex daemon frame is too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.reader, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(w.reader, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, opcode, payload, nil
}
