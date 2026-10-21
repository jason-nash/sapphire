package vsock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"sapphire/engine/internal/protocol"
)

const (
	ControlPort = 1024
	DockerPort  = 1025
	TCPPort     = 1026
)

type Client struct {
	Path string
}

// bufferedConn retains data that arrived in the same read as the OK line.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *bufferedConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}

func (c Client) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	if port == 0 {
		return nil, errors.New("vsock port must be nonzero")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	conn.SetDeadline(deadline)
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readLine(reader, 4096)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	if string(line) != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %s", line)
	}
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func (c Client) Call(ctx context.Context, request protocol.Request) (protocol.Response, error) {
	conn, err := c.Dial(ctx, ControlPort)
	if err != nil {
		return protocol.Response{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return protocol.Response{}, err
	}
	line, err := readLine(bufio.NewReader(conn), protocol.MaxMessage)
	if err != nil {
		return protocol.Response{}, err
	}
	var response protocol.Response
	if err := protocol.Decode(bytes.NewReader(line), &response); err != nil {
		return protocol.Response{}, fmt.Errorf("invalid guest response: %w", err)
	}
	if len(response.Result) == 0 {
		response.Result = json.RawMessage("null")
	}
	return response, nil
}

type Target struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (t Target) Validate() error {
	if t.Host == "" || len(t.Host) > 253 || t.Port < 1 || t.Port > 65535 {
		return errors.New("target requires a host and port between 1 and 65535")
	}
	for _, char := range t.Host {
		if char <= ' ' || char == '/' || char == '\\' {
			return errors.New("invalid target host")
		}
	}
	return nil
}

func (c Client) DialTCP(ctx context.Context, target Target) (net.Conn, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	conn, err := c.Dial(ctx, TCPPort)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(target); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetWriteDeadline(time.Time{})
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, errors.New("protocol line too long")
		}
		line = append(line, fragment...)
		if err == nil {
			return line[:len(line)-1], nil
		}
		if err != bufio.ErrBufferFull {
			if err == io.EOF {
				return nil, errors.New("protocol line missing newline")
			}
			return nil, err
		}
	}
}
