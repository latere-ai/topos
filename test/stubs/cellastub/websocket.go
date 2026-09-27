// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// The opcodes of RFC 6455 the exec socket uses.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// acceptGUID is the constant RFC 6455 computes the handshake's answer over.
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// subprotocol is the exec socket's, cella.exec.v1.
const subprotocol = "cella.exec.v1"

// maxMessage bounds one message the stub reads, Cella's own bound.
const maxMessage = 1 << 20

// The close codes Cella ends a session with.
const (
	closeNormal   = 1000
	closeInternal = 1011
)

// wsConn is the server side of one socket: frames from the client arrive
// masked, frames to it are sent unmasked, one writer at a time.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

// upgrade answers the WebSocket handshake and takes the connection over.
func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" {
		return nil, errors.New("the request is not a WebSocket upgrade")
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + acceptGUID))
	answer := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n" +
		"Sec-WebSocket-Protocol: " + subprotocol + "\r\n\r\n"
	if _, err := io.WriteString(conn, answer); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return &wsConn{conn: conn, br: brw.Reader}, nil
}

// read returns one whole message; a ping is answered, a pong dropped,
// and a close ends the read with io.EOF.
func (c *wsConn) read() (int, []byte, error) {
	var msg []byte
	kind := 0
	for {
		final, op, payload, err := c.frame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			return 0, nil, io.EOF
		case opText, opBinary:
			kind, msg = op, payload
		case opContinuation:
			msg = append(msg, payload...)
		default:
			return 0, nil, fmt.Errorf("the client sent opcode %d", op)
		}
		if len(msg) > maxMessage {
			return 0, nil, errors.New("the client sent a message past the bound")
		}
		if final {
			return kind, msg, nil
		}
	}
}

func (c *wsConn) frame() (bool, int, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	final, op, masked := h[0]&0x80 != 0, int(h[0]&0x0f), h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > maxMessage {
		return false, 0, nil, errors.New("the client sent a frame past the bound")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return final, op, payload, nil
}

// write sends one message as one unmasked frame.
func (c *wsConn) write(op int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	h := []byte{byte(0x80 | op)}
	switch n := len(payload); {
	case n < 126:
		h = append(h, byte(n))
	case n <= 0xffff:
		h = append(h, 126, 0, 0)
		binary.BigEndian.PutUint16(h[2:], uint16(n))
	default:
		h = append(h, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(h[2:], uint64(n))
	}
	_, err := c.conn.Write(append(h, payload...))
	return err
}

// closeWith sends the close frame with a code and a reason, then ends the
// connection.
func (c *wsConn) closeWith(code int, reason string) error {
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	werr := c.write(opClose, append(payload, reason...))
	return errors.Join(werr, c.conn.Close())
}
