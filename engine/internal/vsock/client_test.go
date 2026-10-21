package vsock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sapphire/engine/internal/protocol"
)

func server(t *testing.T, handle func(net.Conn)) Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "vsock.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			handle(conn)
		}
	}()
	return Client{Path: path}
}

func TestConnectRetainsCoalescedPayload(t *testing.T) {
	seen := make(chan string, 1)
	client := server(t, func(conn net.Conn) {
		line, _ := bufio.NewReader(conn).ReadString('\n')
		seen <- line
		io.WriteString(conn, "OK\npayload")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := client.Dial(ctx, DockerPort)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload, err := io.ReadAll(conn)
	if err != nil || string(payload) != "payload" || <-seen != "CONNECT 1025\n" {
		t.Fatalf("lost buffered bytes: %q %v", payload, err)
	}
}

func TestControlPreservesDestinationAndGuestFailure(t *testing.T) {
	requests := make(chan protocol.Request, 1)
	client := server(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		reader.ReadString('\n')
		io.WriteString(conn, "OK\n")
		var request protocol.Request
		json.NewDecoder(reader).Decode(&request)
		requests <- request
		io.WriteString(conn, "{\"ok\":false,\"result\":null,\"error\":\"fixture failure\"}\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.Call(ctx, protocol.Request{Action: "machine.clone", Name: "old", Destination: "new"})
	if err != nil || response.OK || response.Error != "fixture failure" {
		t.Fatalf("guest failure lost: %+v %v", response, err)
	}
	if request := <-requests; request.Destination != "new" {
		t.Fatalf("lost clone destination: %+v", request)
	}
}

func TestConnectIsBoundedAndRejectsBadLines(t *testing.T) {
	for name, reply := range map[string]string{
		"rejected": "ERR no guest\n", "oversized": strings.Repeat("x", 4097) + "\n", "missing newline": "OK",
	} {
		t.Run(name, func(t *testing.T) {
			client := server(t, func(conn net.Conn) {
				bufio.NewReader(conn).ReadString('\n')
				io.WriteString(conn, reply)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if conn, err := client.Dial(ctx, ControlPort); err == nil {
				conn.Close()
				t.Fatal("accepted invalid CONNECT response")
			}
		})
	}
	client := server(t, func(conn net.Conn) {
		io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	if conn, err := client.Dial(ctx, ControlPort); err == nil {
		conn.Close()
		t.Fatal("handshake ignored cancellation")
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancellation did not interrupt read")
	}
}

func TestTCPProxyHeaderThenRawStream(t *testing.T) {
	seen := make(chan Target, 1)
	client := server(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		line, _ := reader.ReadString('\n')
		if line != "CONNECT 1026\n" {
			return
		}
		io.WriteString(conn, "OK\n")
		line, _ = reader.ReadString('\n')
		var target Target
		json.Unmarshal([]byte(line), &target)
		seen <- target
		fmt.Fprint(conn, "raw-stream")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := client.DialTCP(ctx, Target{Host: "127.0.0.1", Port: 6443})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data, err := io.ReadAll(conn)
	target := <-seen
	if err != nil || string(data) != "raw-stream" || target.Port != 6443 || target.Host != "127.0.0.1" {
		t.Fatalf("TCP tunnel: %q %+v %v", data, target, err)
	}
}
