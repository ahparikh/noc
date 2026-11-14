package codex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// serveUpgrade reads the client's upgrade request from br and replies with the
// response built by respond from the client's key.
func serveUpgrade(w io.Writer, br *bufio.Reader, respond func(key string) string) (*http.Request, error) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, err
	}
	_, err = io.WriteString(w, respond(req.Header.Get("Sec-WebSocket-Key")))
	return req, err
}

func okUpgrade(key string) string {
	return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + webSocketAccept(key) + "\r\n\r\n"
}

func serverFrame(t *testing.T, conn net.Conn, fin bool, opcode byte, payload string) {
	t.Helper()
	if err := writeWebSocketFrame(conn, fin, opcode, []byte(payload), false); err != nil {
		t.Errorf("server write: %v", err)
	}
}

func TestWebSocketMessageTransportHandshakeMaskingAndFragmentation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error, 1)
	go func() {
		done <- func() error {
			br := bufio.NewReader(server)
			req, err := serveUpgrade(server, br, okUpgrade)
			if err != nil {
				return err
			}
			if req.Method != "GET" || req.URL.Path != "/" || req.Host != "localhost" {
				return fmt.Errorf("request line = %s %s host %s", req.Method, req.URL.Path, req.Host)
			}
			if !headerContainsToken(req.Header.Get("Connection"), "upgrade") ||
				!headerContainsToken(req.Header.Get("Upgrade"), "websocket") ||
				req.Header.Get("Sec-WebSocket-Version") != "13" || req.Header.Get("Sec-WebSocket-Key") == "" {
				return fmt.Errorf("headers = %v", req.Header)
			}
			for _, h := range []string{"Origin", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions", "Authorization"} {
				if req.Header.Get(h) != "" {
					return fmt.Errorf("unexpected header %s", h)
				}
			}

			// Ping must be answered with a masked pong carrying the payload.
			if err := writeWebSocketFrame(server, true, opPing, []byte("p"), false); err != nil {
				return err
			}
			pong, err := readWebSocketFrame(br, true)
			if err != nil || pong.opcode != opPong || string(pong.payload) != "p" {
				return fmt.Errorf("pong = %+v, %v", pong, err)
			}
			// Fragmented text with an interleaved ping.
			if err := writeWebSocketFrame(server, false, opText, []byte("hel"), false); err != nil {
				return err
			}
			if err := writeWebSocketFrame(server, true, opPing, []byte("q"), false); err != nil {
				return err
			}
			if pong, err = readWebSocketFrame(br, true); err != nil || pong.opcode != opPong || string(pong.payload) != "q" {
				return fmt.Errorf("pong2 = %+v, %v", pong, err)
			}
			if err := writeWebSocketFrame(server, true, opContinuation, []byte("lo"), false); err != nil {
				return err
			}
			// The client's message must be one masked final text frame.
			var head [2]byte
			if _, err := io.ReadFull(br, head[:]); err != nil {
				return err
			}
			if head[0] != 0x81 || head[1]&0x80 == 0 {
				return fmt.Errorf("client frame head = % x", head)
			}
			var mask [4]byte
			if _, err := io.ReadFull(br, mask[:]); err != nil {
				return err
			}
			payload := make([]byte, head[1]&0x7F)
			if _, err := io.ReadFull(br, payload); err != nil {
				return err
			}
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
			if string(payload) != "ack" {
				return fmt.Errorf("payload = %q", payload)
			}
			// Close is echoed and surfaces as EOF.
			if err := writeWebSocketFrame(server, true, opClose, []byte{0x03, 0xe8}, false); err != nil {
				return err
			}
			closeFrame, err := readWebSocketFrame(br, true)
			if err != nil || closeFrame.opcode != opClose || !bytes.Equal(closeFrame.payload, []byte{0x03, 0xe8}) {
				return fmt.Errorf("close echo = %+v, %v", closeFrame, err)
			}
			return nil
		}()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := newWebSocketMessageTransport(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := tr.ReadMessage(ctx)
	if err != nil || string(msg) != "hello" {
		t.Fatalf("message = %q, %v", msg, err)
	}
	if err := tr.WriteMessage(ctx, []byte("ack")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ReadMessage(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("close err = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketMessageTransportRejectsInvalidUpgrade(t *testing.T) {
	good := func(key string) string { return okUpgrade(key) }
	cases := []struct {
		name    string
		respond func(key string) string
		want    string
	}{
		{"status", func(string) string { return "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n" }, "WebSocket upgrade returned 200 OK"},
		{"headers", func(key string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nSec-WebSocket-Accept: " + webSocketAccept(key) + "\r\n\r\n"
		}, "WebSocket upgrade response omitted required headers"},
		{"accept", func(string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Accept: bogus\r\n\r\n"
		}, "WebSocket upgrade response had an invalid accept value"},
		{"extension", func(key string) string {
			return strings.Replace(good(key), "\r\n\r\n", "\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n", 1)
		}, "WebSocket server selected an extension noc did not request"},
		{"garbage", func(string) string { return "nonsense\r\n\r\n" }, "read WebSocket upgrade response: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			go func() {
				defer server.Close()
				_, _ = serveUpgrade(server, bufio.NewReader(server), tc.respond)
			}()
			_, err := newWebSocketMessageTransport(context.Background(), client)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want prefix %q", err, tc.want)
			}
		})
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newWebSocketMessageTransport(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestWebSocketFrameLengths(t *testing.T) {
	for _, n := range []int{0, 1, 125, 126, 127, 65535, 65536, 100000} {
		for _, masked := range []bool{false, true} {
			payload := bytes.Repeat([]byte("x"), n)
			var buf bytes.Buffer
			if err := writeWebSocketFrame(&buf, true, opText, payload, masked); err != nil {
				t.Fatalf("len %d: %v", n, err)
			}
			wantHeader := 2
			switch {
			case n > 65535:
				wantHeader += 8
			case n > 125:
				wantHeader += 2
			}
			if masked {
				wantHeader += 4
			}
			if buf.Len() != wantHeader+n {
				t.Fatalf("len %d masked=%v: encoded %d bytes, want %d", n, masked, buf.Len(), wantHeader+n)
			}
			frame, err := readWebSocketFrame(&buf, masked)
			if err != nil || !frame.fin || frame.opcode != opText || !bytes.Equal(frame.payload, payload) {
				t.Fatalf("len %d masked=%v: %+v, %v", n, masked, frame, err)
			}
		}
	}
}

func TestWriteWebSocketFrameRefusals(t *testing.T) {
	var buf bytes.Buffer
	if err := writeWebSocketFrame(&buf, true, opPing, make([]byte, 126), true); err == nil || err.Error() != "refusing to write an invalid WebSocket control frame" {
		t.Fatalf("err = %v", err)
	}
	if err := writeWebSocketFrame(&buf, false, opPing, nil, true); err == nil {
		t.Fatal("non-final control frame accepted")
	}
	if err := writeWebSocketFrame(&buf, true, opText, make([]byte, maxWebSocketBytes+1), true); err == nil || err.Error() != "WebSocket frame exceeds 16777216 bytes" {
		t.Fatalf("err = %v", err)
	}
	tr := &webSocketMessageTransport{bw: bufio.NewWriter(io.Discard)}
	if err := tr.WriteMessage(context.Background(), []byte{0xff, 0xfe}); err == nil ||
		err.Error() != "refusing to write a WebSocket text frame with invalid UTF-8" {
		t.Fatalf("err = %v", err)
	}
}

// readerTransport builds a transport over pre-encoded server frames.
func readerTransport(frames []byte) (*webSocketMessageTransport, *bytes.Buffer) {
	written := &bytes.Buffer{}
	return &webSocketMessageTransport{br: bufio.NewReader(bytes.NewReader(frames)), bw: bufio.NewWriter(written)}, written
}

func encode(t *testing.T, fin bool, opcode byte, payload []byte, masked bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeWebSocketFrame(&buf, fin, opcode, payload, masked); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWebSocketReaderRejections(t *testing.T) {
	rsv := encode(t, true, opText, []byte("x"), false)
	rsv[0] |= 0x40
	nonCanonical := []byte{0x81, 126, 0, 5}
	badLen64 := []byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0, 5}
	topBit := []byte{0x81, 127, 0x80, 0, 0, 0, 0, 1, 0, 0}
	oversize := []byte{0x81, 127, 0, 0, 0, 0, 0x01, 0, 0, 1}
	badControl := []byte{0x89, 126, 0, 126}
	nonFinalPing := []byte{0x09, 0}
	unknown := []byte{0x83, 0}

	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	cases := []struct {
		name   string
		frames []byte
		want   string
	}{
		{"masked server frame", encode(t, true, opText, []byte("x"), true), "received WebSocket frame with masked=true, want false"},
		{"rsv bits", rsv, "received a WebSocket frame with unsupported reserved bits"},
		{"binary", encode(t, true, opBinary, []byte("x"), false), "received an unsupported WebSocket binary message"},
		{"invalid utf8", encode(t, true, opText, []byte{0xff, 0xfe}, false), "received a WebSocket text frame with invalid UTF-8"},
		{"invalid utf8 fragmented", cat(
			encode(t, false, opText, []byte{0xe2, 0x82}, false),
			encode(t, true, opContinuation, []byte{0x41}, false)),
			"received a fragmented WebSocket text message with invalid UTF-8"},
		{"non-canonical length", nonCanonical, "received a non-canonical WebSocket frame length"},
		{"small 64-bit length", badLen64, "received an invalid WebSocket frame length"},
		{"64-bit top bit", topBit, "received an invalid WebSocket frame length"},
		{"oversize frame", oversize, "WebSocket frame exceeds 16777216 bytes"},
		{"oversize control", badControl, "received an invalid WebSocket control frame"},
		{"non-final control", nonFinalPing, "received an invalid WebSocket control frame"},
		{"unknown opcode", unknown, "received unsupported WebSocket opcode 0x3"},
		{"stray continuation", encode(t, true, opContinuation, []byte("x"), false), "received an unexpected WebSocket continuation frame"},
		{"data during fragment", cat(
			encode(t, false, opText, []byte("a"), false),
			encode(t, true, opText, []byte("b"), false)),
			"received a new WebSocket data frame during a fragmented message"},
		{"oversize message", cat(
			encode(t, false, opText, bytes.Repeat([]byte("a"), 10<<20), false),
			encode(t, true, opContinuation, bytes.Repeat([]byte("a"), 7<<20), false)),
			"WebSocket message exceeds 16777216 bytes"},
		{"eof", nil, "EOF"},
		{"short header", []byte{0x81}, "unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, _ := readerTransport(tc.frames)
			_, err := tr.ReadMessage(context.Background())
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestWebSocketReaderIgnoresPongAndChecksContext(t *testing.T) {
	frames := append(encode(t, true, opPong, []byte("z"), false), encode(t, true, opText, []byte("ok"), false)...)
	tr, _ := readerTransport(frames)
	msg, err := tr.ReadMessage(context.Background())
	if err != nil || string(msg) != "ok" {
		t.Fatalf("msg = %q, %v", msg, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr, _ = readerTransport(frames)
	if _, err := tr.ReadMessage(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
