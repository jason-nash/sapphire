package forward

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sapphire/engine/internal/vsock"
)

func TestDockerInitialSnapshotAndEventsUsePublishedHostPort(t *testing.T) {
	hosts := []string{
		"127.0.0.1", "127.0.0.1", "127.0.0.1", "::1",
		"127.0.0.2", "192.168.128.2",
	}
	hostPorts := make([]int, len(hosts))
	wanted := make(map[int]Spec)
	var reservations []net.Listener
	for i, host := range hosts {
		reservation, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer reservation.Close()
		reservations = append(reservations, reservation)
		port := reservation.Addr().(*net.TCPAddr).Port
		hostPorts[i] = port
		wanted[port] = Spec{Host: host, Port: port, HostPort: port, Source: "docker"}
	}
	for _, reservation := range reservations {
		reservation.Close()
	}
	containers := []map[string]any{
		{"Id": "one", "Ports": []map[string]any{
			{"IP": "0.0.0.0", "PrivatePort": 80, "PublicPort": hostPorts[0], "Type": "tcp"},
			{"PrivatePort": 443, "PublicPort": hostPorts[1], "Type": "tcp"},
			{"IP": "::", "PrivatePort": 8080, "PublicPort": hostPorts[2], "Type": "tcp"},
			{"IP": "192.0.2.53", "PrivatePort": 53, "PublicPort": hostPorts[0], "Type": "udp"},
			{"IP": "192.0.2.99", "PrivatePort": 80, "PublicPort": hostPorts[0], "Type": "sctp"},
			{"IP": "0.0.0.0", "PrivatePort": 9000, "Type": "tcp"},
		}},
		{"Id": "two", "Ports": []map[string]any{
			{"IP": "::1", "PrivatePort": 22, "PublicPort": hostPorts[3], "Type": "tcp"},
			{"IP": "127.0.0.2", "PrivatePort": 8081, "PublicPort": hostPorts[4], "Type": "tcp"},
			{"IP": "192.168.128.2", "PrivatePort": 9090, "PublicPort": hostPorts[5], "Type": "tcp"},
			{"IP": "not-an-ip", "PrivatePort": 80, "PublicPort": 55001, "Type": "tcp"},
			{"IP": "0.0.0.0", "PrivatePort": 80, "PublicPort": -1, "Type": "tcp"},
			{"IP": "0.0.0.0", "PrivatePort": 80, "PublicPort": 65536, "Type": "tcp"},
			{"IP": "192.0.2.1", "PrivatePort": 22, "PublicPort": hostPorts[3], "Type": "TCP"},
			{"IP": "192.0.2.1", "PrivatePort": 9090, "PublicPort": hostPorts[5]},
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := New(ctx, nil)
	defer manager.Close()
	events := make(chan string, 2)
	var lists, inspections atomic.Int32
	var running atomic.Bool
	running.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/events":
			if got := r.URL.Query().Get("filters"); got != `{"type":["container"]}` {
				t.Errorf("unexpected event filters: %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			for {
				select {
				case event := <-events:
					fmt.Fprintln(w, event)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case r.URL.Path == "/containers/json":
			lists.Add(1)
			if running.Load() {
				json.NewEncoder(w).Encode(containers)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case strings.HasPrefix(r.URL.Path, "/containers/"):
			inspections.Add(1)
			http.Error(w, "container inspection is unnecessary", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer cancel()
	watch := NewDockerWatch(vsock.Client{}, manager, func(err error) {
		if !strings.Contains(err.Error(), "non-TCP host publications are unsupported") {
			t.Errorf("watcher: %v", err)
		}
	})
	defer watch.Stop()
	watch.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", strings.TrimPrefix(server.URL, "http://"))
	}
	watch.Start(ctx)
	waitFor(t, func() bool { return len(manager.List()) == len(wanted) })
	for _, spec := range manager.List() {
		if want, ok := wanted[spec.HostPort]; !ok || spec != want {
			t.Fatalf("unexpected published-port forward: got %+v, want %+v", spec, want)
		}
	}
	initial := lists.Load()
	if initial != 1 {
		t.Fatalf("initial snapshot made %d list requests, want 1", initial)
	}
	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
	}
	if lists.Load() != initial {
		t.Fatal("idle Docker watcher polled container state")
	}
	running.Store(false)
	events <- `{"Action":"die","Type":"container"}`
	waitFor(t, func() bool { return len(manager.List()) == 0 })
	if lists.Load() != initial+1 {
		t.Fatal("event did not trigger exactly one state refresh")
	}
	if got := inspections.Load(); got != 0 {
		t.Fatalf("made %d unnecessary container inspections", got)
	}
}

func TestDockerFailedInitialSnapshotsKeepBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager := New(ctx, nil)
	defer manager.Close()
	var streams, lists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			streams.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/containers/json":
			lists.Add(1)
			http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected Docker request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	watch := NewDockerWatch(vsock.Client{}, manager, func(err error) { t.Error(err) })
	defer watch.transport.CloseIdleConnections()
	watch.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", strings.TrimPrefix(server.URL, "http://"))
	}
	var delay time.Duration
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		duration, err := watch.session(ctx)
		if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatalf("expected initial inventory failure, got %v", err)
		}
		if duration != 0 {
			t.Fatalf("failed initial inventory counted as a healthy session: %v", duration)
		}
		delay = dockerRetryDelay(delay, duration)
		if delay != want {
			t.Fatalf("retry delay after inventory failure: got %v, want %v", delay, want)
		}
	}
	if streams.Load() != 3 || lists.Load() != 3 {
		t.Fatalf("expected three opened streams and failed snapshots, got %d and %d", streams.Load(), lists.Load())
	}
}

func TestDockerRetryDelay(t *testing.T) {
	tests := []struct {
		name     string
		previous time.Duration
		session  time.Duration
		want     time.Duration
	}{
		{"first failure", 0, 0, time.Second},
		{"short session", time.Second, time.Second, 2 * time.Second},
		{"repeated failure", 4 * time.Second, 0, 8 * time.Second},
		{"cap", 16 * time.Second, 0, 30 * time.Second},
		{"stay capped", 30 * time.Second, 0, 30 * time.Second},
		{"below healthy threshold", 30 * time.Second, 30*time.Second - time.Nanosecond, 30 * time.Second},
		{"healthy threshold", 30 * time.Second, 30 * time.Second, time.Second},
		{"sustained healthy session", 30 * time.Second, time.Minute, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dockerRetryDelay(tt.previous, tt.session); got != tt.want {
				t.Fatalf("retry delay: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStoppedWatcherCannotBeRestartedByInFlightInitialProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := New(ctx, nil)
	defer manager.Close()
	probed, finish := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			close(probed)
			<-finish
			fmt.Fprint(w, "OK")
		} else {
			t.Error("stopped watcher started an event stream")
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	watch := NewDockerWatch(vsock.Client{}, manager, func(err error) { t.Error(err) })
	defer watch.Stop()
	watch.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", strings.TrimPrefix(server.URL, "http://"))
	}
	done := make(chan struct{})
	go func() { watch.TryStart(ctx); close(done) }()
	select {
	case <-probed:
	case <-time.After(time.Second):
		t.Fatal("initial probe did not start")
	}
	watch.Stop()
	close(finish)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe did not finish")
	}
	watch.mu.Lock()
	started := watch.cancel != nil
	watch.mu.Unlock()
	if started {
		t.Fatal("in-flight initial probe restarted watcher after stop")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("condition did not become true")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Model the guest endpoint with an IPv6-only socket, while the host forward
// remains private IPv4. The target uses the same JSON record as DialTCP.
func TestDockerIPv6LoopbackForward(t *testing.T) {
	guest, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	port := guest.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
		conn.Close()
		t.Fatal("IPv4 negative control unexpectedly connected")
	}
	go func() {
		conn, err := guest.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "ipv6-only")
	}()
	manager := New(ctx, func(ctx context.Context, target vsock.Target) (net.Conn, error) {
		record, err := json.Marshal(target)
		if err != nil {
			return nil, err
		}
		var request vsock.Target
		if err := json.Unmarshal(record, &request); err != nil {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(request.Host, strconv.Itoa(request.Port)))
	})
	defer manager.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"Ports": []map[string]any{{"IP": "::1", "PrivatePort": 80, "PublicPort": port, "Type": "tcp"}}}})
	}))
	defer server.Close()
	watch := NewDockerWatch(vsock.Client{}, manager, func(err error) { t.Error(err) })
	defer watch.Stop()
	watch.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", strings.TrimPrefix(server.URL, "http://"))
	}
	if err := watch.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "ipv6-only" {
		t.Fatalf("forward got %q, %v", got, err)
	}
}

func TestUnsupportedPublicationsWarnWithoutBreakingTCPOrPolling(t *testing.T) {
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	reservation.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager := New(ctx, func(ctx context.Context, target vsock.Target) (net.Conn, error) {
		if target.Host != "127.0.0.1" || target.Port != port {
			return nil, fmt.Errorf("wrong TCP target: %+v", target)
		}
		host, guest := net.Pipe()
		go func() {
			defer guest.Close()
			io.WriteString(guest, "tcp")
		}()
		return host, nil
	})
	defer manager.Close()
	var published atomic.Bool
	published.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ports := []map[string]any{
			{"IP": "127.0.0.1", "PublicPort": port, "Type": "tcp"},
			{"PrivatePort": 67, "Type": "udp"},
		}
		if published.Load() {
			ports = append(ports, map[string]any{"IP": "127.0.0.1", "PublicPort": 53, "Type": "udp"})
		}
		json.NewEncoder(w).Encode([]map[string]any{{"Ports": ports}})
	}))
	defer server.Close()
	var warnings []error
	watch := NewDockerWatch(vsock.Client{}, manager, func(err error) { warnings = append(warnings, err) })
	defer watch.transport.CloseIdleConnections()
	watch.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", strings.TrimPrefix(server.URL, "http://"))
	}
	for i := 0; i < 2; i++ {
		if err := watch.refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "UDP") {
		t.Fatalf("unsupported UDP must warn once across repeated snapshots: %v", warnings)
	}
	conn, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || string(got) != "tcp" {
		t.Fatalf("supported TCP forwarding failed: %q, %v", got, err)
	}
	published.Store(false)
	if err := watch.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("unpublished UDP must not warn: %v", warnings)
	}
	published.Store(true)
	if err := watch.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 2 {
		t.Fatalf("new unsupported publication was not reported: %v", warnings)
	}
}
