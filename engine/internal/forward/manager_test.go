package forward

import (
	"context"
	"net"
	"strconv"
	"testing"

	"sapphire/engine/internal/vsock"
)

func TestForwardOwnershipAndPortConflicts(t *testing.T) {
	manager := New(context.Background(), func(context.Context, vsock.Target) (net.Conn, error) {
		panic("this test must not establish a guest connection")
	})
	defer manager.Close()
	first, err := manager.Add(Spec{Host: "127.0.0.1", Port: 80, Source: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(Spec{Host: "127.0.0.1", Port: 90, HostPort: first.HostPort, Source: "docker"}); err == nil {
		t.Fatal("silently replaced manual forward")
	}
	if err := manager.Sync("docker", nil); err != nil {
		t.Fatal(err)
	}
	if len(manager.List()) != 1 {
		t.Fatal("Docker sync deleted manual listener")
	}
	if err := manager.Remove(first.HostPort, "docker"); err == nil {
		t.Fatal("removed another source's listener")
	}
	if err := manager.Remove(first.HostPort, "manual"); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(first.HostPort)))
	if err != nil {
		t.Fatalf("listener leaked: %v", err)
	}
	listener.Close()
}

func TestSyncReplacesOnlyOwnedTarget(t *testing.T) {
	manager := New(context.Background(), nil)
	defer manager.Close()
	spec, err := manager.Add(Spec{Host: "127.0.0.1", Port: 80, Source: "docker"})
	if err != nil {
		t.Fatal(err)
	}
	spec.Port = 81
	if err := manager.Sync("docker", []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	got := manager.List()
	if len(got) != 1 || got[0].Port != 81 {
		t.Fatalf("stale target: %+v", got)
	}
}
