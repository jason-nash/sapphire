package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGuestProcessHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--guest-helper" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "exit":
		fmt.Fprint(os.Stdout, "stdout")
		fmt.Fprint(os.Stderr, "stderr")
		os.Exit(23)
	case "large":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte{0}, MaxOutputSize+100))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte{1}, MaxOutputSize+100))
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestSupervisorCaptureAndCancellation(t *testing.T) {
	s := newSupervisor(readBootID())
	run := func(ctx context.Context, mode string) (CommandResult, error) {
		return s.capture(ctx, os.Args[0], []string{"-test.run=^TestGuestProcessHelper$", "--", "--guest-helper", mode}, nil, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 12 {
		result, err := run(ctx, "exit")
		if err != nil || result.ExitCode != 23 || result.Stdout != "stdout" || result.Stderr != "stderr" {
			t.Fatalf("fast child exit: %+v %v", result, err)
		}
	}
	result, err := run(ctx, "large")
	if err != nil || !result.Truncated || len(result.Stdout) != MaxOutputSize || len(result.Stderr) != MaxOutputSize {
		t.Fatalf("large child output: stdout=%d stderr=%d truncated=%v err=%v", len(result.Stdout), len(result.Stderr), result.Truncated, err)
	}
	var wire bytes.Buffer
	if err := writeResponse(&wire, Response{OK: true, Result: result}); err != nil || wire.Len() >= MaxResponseSize {
		t.Fatalf("worst escaped command result exceeds wire limit: %d %v", wire.Len(), err)
	}
	cancelled, stop := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, stop)
	if _, err := run(cancelled, "sleep"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled child: %v", err)
	}
	client, server := net.Pipe()
	defer server.Close()
	disconnected, disconnect := context.WithCancel(ctx)
	defer disconnect()
	cancelOnDisconnect(server, disconnect)
	time.AfterFunc(50*time.Millisecond, func() { client.Close() })
	started := time.Now()
	if _, err := run(disconnected, "sleep"); !errors.Is(err, context.Canceled) {
		t.Fatalf("connection closure did not cancel active command: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("disconnected command did not stop within cancellation grace")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.children) != 0 {
		t.Fatal("cancelled command left unreaped supervised children")
	}
}

func TestWaitingMachineLockIsCancellable(t *testing.T) {
	m := &machines{}
	unlock, err := m.lock(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(25*time.Millisecond, cancel)
	started := time.Now()
	if _, err := m.lock(ctx, "example"); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting machine lock: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled request kept waiting on the machine lock")
	}
}

func TestResponseLimitAndShutdownAcknowledgement(t *testing.T) {
	var wire bytes.Buffer
	if err := writeResponse(&wire, Response{OK: true, Result: strings.Repeat("x", MaxResponseSize)}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(bytes.TrimSpace(wire.Bytes()), &response); err != nil || response.OK || response.Error == "" {
		t.Fatalf("oversized response must be a readable error: %+v %v", response, err)
	}
	a := &agent{shutdown: make(chan struct{}, 1)}
	client, server := net.Pipe()
	defer client.Close()
	go a.serveRPC(context.Background(), server)
	client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte("{\"action\":\"shutdown\"}\n")); err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(client).Decode(&response); err != nil || !response.OK {
		t.Fatalf("shutdown acknowledgement: %+v %v", response, err)
	}
	select {
	case <-a.shutdown:
	case <-time.After(time.Second):
		t.Fatal("acknowledged shutdown did not notify PID 1")
	}
}

func TestKubernetesContainerOwnership(t *testing.T) {
	uid := "12345678-1234-1234-1234-123456789abc"
	container := dockerInspection{Name: "/k8s_POD_example_default_" + uid + "_0"}
	container.Config.Labels = map[string]string{
		"io.kubernetes.pod.uid": uid, "io.kubernetes.pod.name": "example",
		"io.kubernetes.pod.namespace": "default", "io.kubernetes.container.name": "POD",
	}
	if !isKubernetesContainer(container) {
		t.Fatal("verified sandbox not recognized")
	}
	container.Name = "/ordinary-user-container"
	if isKubernetesContainer(container) {
		t.Fatal("ordinary Docker container accepted")
	}
	container.Name = "/k8s_POD_example_default_" + uid + "_0"
	delete(container.Config.Labels, "io.kubernetes.pod.namespace")
	if isKubernetesContainer(container) {
		t.Fatal("partial Kubernetes label set accepted")
	}
}

func TestDisconnectCancelsRequest(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnDisconnect(server, cancel)
	client.Close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("closed RPC connection did not cancel the request context")
	}
}
