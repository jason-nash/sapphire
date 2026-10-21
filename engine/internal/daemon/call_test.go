package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"sapphire/engine/internal/forward"
	"sapphire/engine/internal/protocol"
	"sapphire/engine/internal/vsock"
)

func TestKubernetesStartWatchesDockerAfterInitialProbeFails(t *testing.T) {
	c := fixtureConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	listener, err := net.Listen("unix", c.Path("vsock.sock"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	defer func() {
		cancel()
		listener.Close()
		handlers.Wait()
	}()
	events, snapshot := make(chan struct{}, 1), make(chan struct{}, 1)
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				reader := bufio.NewReader(conn)
				line, _ := reader.ReadString('\n')
				io.WriteString(conn, "OK\n")
				switch line {
				case "CONNECT 1024\n":
					var request protocol.Request
					if err := json.NewDecoder(reader).Decode(&request); err != nil || request.Action != "k8s.start" {
						t.Errorf("unexpected guest control request: %+v, %v", request, err)
						return
					}
					json.NewEncoder(conn).Encode(protocol.Success(nil))
				case "CONNECT 1025\n":
					request, err := http.ReadRequest(reader)
					if err != nil {
						return
					}
					request.Body.Close()
					switch request.URL.Path {
					case "/_ping":
						io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
					case "/events":
						io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n")
						events <- struct{}{}
						io.Copy(io.Discard, reader)
					case "/containers/json":
						io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n[]")
						snapshot <- struct{}{}
					}
				}
			}()
		}
	}()
	d := &Daemon{
		config: c, ctx: ctx, guest: vsock.Client{Path: c.Path("vsock.sock")},
		child:  &child{done: make(chan struct{})},
		status: HostStatus{Running: true, State: "running"},
	}
	d.forwards = forward.New(ctx, d.guest.DialTCP)
	defer d.forwards.Close()
	d.docker = forward.NewDockerWatch(d.guest, d.forwards, func(err error) { t.Errorf("Docker watcher: %v", err) })
	defer d.docker.Stop()
	// Docker is stopped when the daemon performs its one-time startup probe.
	d.docker.TryStart(ctx)
	select {
	case <-events:
		t.Fatal("failed startup probe started the watcher")
	default:
	}
	response := d.call(ctx, protocol.Request{Action: "k8s.start"})
	if !response.OK {
		if strings.Contains(response.Error, "bind: address already in use") {
			t.Skipf("localhost:6443 unavailable for Kubernetes forward: %s", response.Error)
		}
		t.Fatalf("Kubernetes start failed: %+v", response)
	}
	for _, observed := range []<-chan struct{}{events, snapshot} {
		select {
		case <-observed:
		case <-ctx.Done():
			t.Fatal("successful Kubernetes start did not subscribe to Docker events and list containers")
		}
	}
}
