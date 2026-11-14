package codex

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
)

// The Codex shared daemon and `codex app-server proxy` both speak an HTTP/1.1
// WebSocket upgrade followed by RFC 6455 text frames; the proxy does not
// translate that stream into JSONL, so one transport serves both. This client
// is deliberately minimal: no extensions, no subprotocols, no client
// fragmentation, unmasked server frames only, UTF-8 validated, 16 MiB caps.

const (
	// maxWebSocketBytes caps a single frame and a reassembled message.
	maxWebSocketBytes = 16 << 20

	webSocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// messageTransport carries one JSON-RPC message per call.
type messageTransport interface {
	ReadMessage(ctx context.Context) ([]byte, error)
	WriteMessage(ctx context.Context, message []byte) error
}

type webSocketMessageTransport struct {
	br *bufio.Reader

	writeMu sync.Mutex
	bw      *bufio.Writer
}

// newWebSocketMessageTransport performs the client upgrade handshake over rw
// and returns a transport for text frames.
func newWebSocketMessageTransport(ctx context.Context, rw io.ReadWriter) (*webSocketMessageTransport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		return nil, fmt.Errorf("generate WebSocket handshake key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])

	bw := bufio.NewWriter(rw)
	request := "GET / HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := bw.WriteString(request); err != nil {
		return nil, fmt.Errorf("write WebSocket upgrade request: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return nil, fmt.Errorf("flush WebSocket upgrade request: %w", err)
	}

	// Keep this buffered reader for frames so bytes that follow the response
	// headers are not lost.
	br := bufio.NewReader(rw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("read WebSocket upgrade response: %w", err)
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("WebSocket upgrade returned %s", resp.Status)
	}
	if !headerContainsToken(resp.Header.Get("Connection"), "upgrade") ||
		!headerContainsToken(resp.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("WebSocket upgrade response omitted required headers")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != webSocketAccept(key) {
		return nil, errors.New("WebSocket upgrade response had an invalid accept value")
	}
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		return nil, errors.New("WebSocket server selected an extension noc did not request")
	}
	return &webSocketMessageTransport{br: br, bw: bw}, nil
}

func webSocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + webSocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func headerContainsToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// ReadMessage returns the next complete text message.
func (t *webSocketMessageTransport) ReadMessage(ctx context.Context) ([]byte, error) {
	var fragment []byte
	fragmented := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		frame, err := readWebSocketFrame(t.br, false)
		if err != nil {
			return nil, err
		}
		switch frame.opcode {
		case opText:
			if fragmented {
				return nil, errors.New("received a new WebSocket data frame during a fragmented message")
			}
			if frame.fin {
				if !utf8.Valid(frame.payload) {
					return nil, errors.New("received a WebSocket text frame with invalid UTF-8")
				}
				return frame.payload, nil
			}
			fragmented = true
			if fragment, err = appendWebSocketFragment(nil, frame.payload); err != nil {
				return nil, err
			}
		case opContinuation:
			if !fragmented {
				return nil, errors.New("received an unexpected WebSocket continuation frame")
			}
			if fragment, err = appendWebSocketFragment(fragment, frame.payload); err != nil {
				return nil, err
			}
			if frame.fin {
				if !utf8.Valid(fragment) {
					return nil, errors.New("received a fragmented WebSocket text message with invalid UTF-8")
				}
				return fragment, nil
			}
		case opPing:
			if err := t.writeFrame(opPong, frame.payload); err != nil {
				return nil, fmt.Errorf("write WebSocket pong: %w", err)
			}
		case opPong:
		case opClose:
			_ = t.writeFrame(opClose, frame.payload) // best effort
			return nil, io.EOF
		case opBinary:
			return nil, errors.New("received an unsupported WebSocket binary message")
		default:
			return nil, fmt.Errorf("received unsupported WebSocket opcode %#x", frame.opcode)
		}
	}
}

func appendWebSocketFragment(buf, payload []byte) ([]byte, error) {
	if len(buf)+len(payload) > maxWebSocketBytes {
		return nil, fmt.Errorf("WebSocket message exceeds %d bytes", maxWebSocketBytes)
	}
	return append(buf, payload...), nil
}

// WriteMessage sends message as one final, masked text frame.
func (t *webSocketMessageTransport) WriteMessage(_ context.Context, message []byte) error {
	if !utf8.Valid(message) {
		return errors.New("refusing to write a WebSocket text frame with invalid UTF-8")
	}
	return t.writeFrame(opText, message)
}

func (t *webSocketMessageTransport) writeFrame(opcode byte, payload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := writeWebSocketFrame(t.bw, true, opcode, payload, true); err != nil {
		return err
	}
	return t.bw.Flush()
}

type webSocketFrame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// readWebSocketFrame reads one frame. wantMasked is true when reading frames
// sent by a client (tests) and false for frames sent by a server.
func readWebSocketFrame(r io.Reader, wantMasked bool) (webSocketFrame, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return webSocketFrame{}, err
	}
	if head[0]&0x70 != 0 {
		return webSocketFrame{}, errors.New("received a WebSocket frame with unsupported reserved bits")
	}
	fin := head[0]&0x80 != 0
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	if masked != wantMasked {
		return webSocketFrame{}, fmt.Errorf("received WebSocket frame with masked=%t, want %t", masked, wantMasked)
	}

	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return webSocketFrame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
		if length < 126 {
			return webSocketFrame{}, errors.New("received a non-canonical WebSocket frame length")
		}
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return webSocketFrame{}, err
		}
		length = binary.BigEndian.Uint64(ext[:])
		if length < 65536 || length&(1<<63) != 0 {
			return webSocketFrame{}, errors.New("received an invalid WebSocket frame length")
		}
	}
	if length > maxWebSocketBytes {
		return webSocketFrame{}, fmt.Errorf("WebSocket frame exceeds %d bytes", maxWebSocketBytes)
	}
	if opcode >= 0x8 && (!fin || length > 125) {
		return webSocketFrame{}, errors.New("received an invalid WebSocket control frame")
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return webSocketFrame{}, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return webSocketFrame{}, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return webSocketFrame{fin: fin, opcode: opcode, payload: payload}, nil
}

// writeWebSocketFrame encodes one frame with the minimal length encoding. A
// masked frame gets a fresh random mask.
func writeWebSocketFrame(w io.Writer, fin bool, opcode byte, payload []byte, masked bool) error {
	if len(payload) > maxWebSocketBytes {
		return fmt.Errorf("WebSocket frame exceeds %d bytes", maxWebSocketBytes)
	}
	if opcode >= 0x8 && (!fin || len(payload) > 125) {
		return errors.New("refusing to write an invalid WebSocket control frame")
	}
	first := opcode
	if fin {
		first |= 0x80
	}
	var maskBit byte
	if masked {
		maskBit = 0x80
	}

	buf := make([]byte, 0, 14+len(payload))
	buf = append(buf, first)
	switch n := len(payload); {
	case n <= 125:
		buf = append(buf, maskBit|byte(n))
	case n <= 65535:
		buf = append(buf, maskBit|126, byte(n>>8), byte(n))
	default:
		buf = append(buf, maskBit|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	if masked {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return fmt.Errorf("generate WebSocket mask: %w", err)
		}
		buf = append(buf, mask[:]...)
		start := len(buf)
		buf = append(buf, payload...)
		for i := start; i < len(buf); i++ {
			buf[i] ^= mask[(i-start)%4]
		}
	} else {
		buf = append(buf, payload...)
	}
	_, err := w.Write(buf)
	return err
}
