package main

// Minimal WebSocket server (RFC 6455), shared by the audio stream and,
// later, the keyboard/mouse input channel.
//
// Supported: handshake, text/binary messages (unfragmented on send,
// fragmented on receive), ping/pong, close. Not supported: extensions
// (permessage-deflate), subprotocol negotiation.

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
	"time"
)

const (
	wsOpCont   = 0x0
	wsOpText   = 0x1
	wsOpBinary = 0x2
	wsOpClose  = 0x8
	wsOpPing   = 0x9
	wsOpPong   = 0xa

	wsMaxMessage   = 64 << 10
	wsWriteTimeout = 5 * time.Second
	wsGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

var errWSClosed = errors.New("websocket closed")

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu    sync.Mutex
	closed bool
}

// wsUpgrade performs the server side of the opening handshake.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !headerHas(r.Header, "Connection", "upgrade") || !headerHas(r.Header, "Upgrade", "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("not a websocket request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("unsupported websocket version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("missing key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return nil, errors.New("hijack unsupported")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return &wsConn{conn: conn, br: brw.Reader}, nil
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// WriteMessage sends one unfragmented message. Safe for concurrent use.
func (c *wsConn) WriteMessage(op byte, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errWSClosed
	}
	return c.writeFrameLocked(op, data)
}

func (c *wsConn) writeFrameLocked(op byte, data []byte) error {
	var hdr [10]byte
	hdr[0] = 0x80 | op // FIN
	n := len(data)
	hl := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
		hl = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
		hl = 10
	}
	c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	bufs := net.Buffers{hdr[:hl], data}
	_, err := bufs.WriteTo(c.conn)
	return err
}

// ReadMessage returns the next text or binary message, answering pings and
// close frames itself. It must be called from a single goroutine.
func (c *wsConn) ReadMessage() (op byte, data []byte, err error) {
	var msgOp byte
	var msg []byte
	for {
		fin, fop, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch fop {
		case wsOpPing:
			if err := c.WriteMessage(wsOpPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			c.wmu.Lock()
			if !c.closed {
				c.writeFrameLocked(wsOpClose, payload[:min(len(payload), 2)])
				c.closed = true
			}
			c.wmu.Unlock()
			return 0, nil, errWSClosed
		case wsOpText, wsOpBinary:
			if msgOp != 0 {
				return 0, nil, errors.New("websocket: new message inside fragmented message")
			}
			msgOp = fop
			msg = payload
		case wsOpCont:
			if msgOp == 0 {
				return 0, nil, errors.New("websocket: unexpected continuation frame")
			}
			if len(msg)+len(payload) > wsMaxMessage {
				return 0, nil, errors.New("websocket: message too large")
			}
			msg = append(msg, payload...)
		default:
			return 0, nil, fmt.Errorf("websocket: unknown opcode %#x", fop)
		}
		if fin {
			return msgOp, msg, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = h[0] & 0x0f
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	masked := h[1]&0x80 != 0
	if !masked {
		return false, 0, nil, errors.New("websocket: client frame not masked")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= 0x8 && (n > 125 || !fin) {
		return false, 0, nil, errors.New("websocket: invalid control frame")
	}
	if n > wsMaxMessage {
		return false, 0, nil, errors.New("websocket: frame too large")
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return
}

// Close sends a close frame (best effort) and closes the connection.
func (c *wsConn) Close() error {
	c.wmu.Lock()
	if !c.closed {
		c.writeFrameLocked(wsOpClose, []byte{0x03, 0xe8}) // 1000 normal closure
		c.closed = true
	}
	c.wmu.Unlock()
	return c.conn.Close()
}
