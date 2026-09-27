// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// wsClient is a raw client of the exec socket, for the frames the Cella
// client never sends.
type wsClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialSocket(t *testing.T, s *Server, id string) *wsClient {
	t.Helper()
	u, err := url.Parse(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	req := "GET /v1/sandboxes/" + id + "/exec HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("handshake %d %v", resp.StatusCode, resp.Header)
	}
	return &wsClient{conn: conn, br: br}
}

func (w *wsClient) send(op int, payload []byte) error { return w.sendFrame(true, op, payload) }

// sendFrame writes one masked frame.
func (w *wsClient) sendFrame(final bool, op int, payload []byte) error {
	b0 := byte(op)
	if final {
		b0 |= 0x80
	}
	h := []byte{b0}
	switch n := len(payload); {
	case n < 126:
		h = append(h, 0x80|byte(n))
	case n <= 0xffff:
		h = append(h, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(h[2:], uint16(n))
	default:
		h = append(h, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(h[2:], uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.conn.Write(append(append(h, mask[:]...), masked...))
	return err
}

// read reads one unmasked frame from the server.
func (w *wsClient) read() (int, []byte, error) {
	c := &wsConn{br: w.br}
	_, op, payload, err := c.frame()
	return op, payload, err
}

func TestWebSocketFrames(t *testing.T) {
	server, peer := net.Pipe()
	c := &wsConn{conn: server, br: bufio.NewReader(server)}
	client := &wsClient{conn: peer, br: bufio.NewReader(peer)}
	errs := make(chan error, 1)
	go func() {
		errs <- errors.Join(
			client.send(opBinary, []byte(strings.Repeat("a", 200))),
			client.send(opBinary, []byte(strings.Repeat("b", 70000))),
			client.sendFrame(true, 0x3, nil),
		)
	}()
	for _, want := range []int{200, 70000} {
		op, payload, err := c.read()
		if err != nil || op != opBinary || len(payload) != want {
			t.Fatalf("read %d %d %v, want %d bytes", op, len(payload), err, want)
		}
	}
	if _, _, err := c.read(); err == nil {
		t.Error("an unknown opcode was read")
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	go func() {
		errs <- errors.Join(c.write(opBinary, []byte(strings.Repeat("c", 300))), c.write(opBinary, []byte(strings.Repeat("d", 70000))))
	}()
	for _, want := range []int{300, 70000} {
		op, payload, err := client.read()
		if err != nil || op != opBinary || len(payload) != want {
			t.Fatalf("read %d %d %v, want %d bytes", op, len(payload), err, want)
		}
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	// A frame past the bound is refused, in its length and in a message
	// joined from frames.
	go func() {
		h := []byte{0x82, 127, 0, 0, 0, 0, 0, 0x20, 0, 0}
		_, err := peer.Write(h)
		errs <- err
	}()
	if _, _, err := c.read(); err == nil {
		t.Error("a frame past the bound was read")
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(server.Close(), peer.Close()); err != nil {
		t.Fatal(err)
	}
	for _, cut := range [][]byte{{0x82}, {0x82, 126}, {0x82, 127, 0}, {0x82, 0x85}, {0x82, 0x85, 1, 2, 3, 4}} {
		c := &wsConn{br: bufio.NewReader(strings.NewReader(string(cut)))}
		if _, _, err := c.read(); err == nil {
			t.Errorf("a cut frame %v was read", cut)
		}
	}
	joined := make([]byte, 0, 2*(maxMessage/2+10))
	for range 2 {
		h := []byte{0x02, 127, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(h[2:], uint64(maxMessage/2+1))
		joined = append(joined, h...)
		joined = append(joined, make([]byte, maxMessage/2+1)...)
	}
	c = &wsConn{br: bufio.NewReader(strings.NewReader(string(joined)))}
	if _, _, err := c.read(); err == nil {
		t.Error("a message past the bound was read")
	}
}

func TestUpgradeRefusesAPlainRequest(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upgrade(nil, req); err == nil {
		t.Error("a request with no upgrade was upgraded")
	}
}
