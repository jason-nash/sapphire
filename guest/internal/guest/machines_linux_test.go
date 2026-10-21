package guest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMachineLockConcurrentExclusionAndRelease(t *testing.T) {
	var m machines
	var active atomic.Int32
	var workers sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 32 {
		workers.Go(func() {
			for range 64 {
				unlock, err := m.lock(ctx, "same-machine")
				if err != nil {
					t.Error(err)
					return
				}
				if got := active.Add(1); got != 1 {
					t.Errorf("%d concurrent holders for one machine", got)
				}
				runtime.Gosched()
				active.Add(-1)
				unlock()
			}
		})
	}
	workers.Wait()
	if len(m.locks) != 0 {
		t.Fatalf("retained %d locks after all holders and waiters left", len(m.locks))
	}
}

func TestMachineLockChurnAndCancellationRelease(t *testing.T) {
	var m machines
	for index := range 1000 {
		name := fmt.Sprintf("machine-%d", index)
		unlock, err := m.lock(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		if len(m.locks) != 0 {
			t.Fatalf("retained lock after releasing %q", name)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := m.lock(cancelled, name); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled acquisition: %v", err)
		}
	}
	for range 32 {
		unlock, err := m.lock(context.Background(), "held-machine")
		if err != nil {
			t.Fatal(err)
		}
		waiting, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		if _, err := m.lock(waiting, "held-machine"); !errors.Is(err, context.DeadlineExceeded) {
			cancel()
			unlock()
			t.Fatalf("waiting acquisition: %v", err)
		}
		cancel()
		if lock := m.locks["held-machine"]; lock == nil || lock.refs != 1 {
			unlock()
			t.Fatalf("cancelled waiter discarded or retained the wrong lock: %+v", lock)
		}
		unlock()
		if len(m.locks) != 0 {
			t.Fatal("retained entry after holder and cancelled waiter left")
		}
	}
}

func TestMachineLockDifferentNamesProceedIndependently(t *testing.T) {
	var m machines
	unlock, err := m.lock(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second, err := m.lock(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	second()
}

func TestDetectInitRejectsIncompleteBusyboxSetup(t *testing.T) {
	const alpineInittab = `::sysinit:/sbin/openrc sysinit
::sysinit:/sbin/openrc boot
::wait:/sbin/openrc default
tty1::respawn:/sbin/getty 38400 tty1
tty2::respawn:/sbin/getty 38400 tty2
tty3::respawn:/sbin/getty 38400 tty3
tty4::respawn:/sbin/getty 38400 tty4
tty5::respawn:/sbin/getty 38400 tty5
tty6::respawn:/sbin/getty 38400 tty6
::ctrlaltdel:/sbin/reboot
::shutdown:/sbin/openrc shutdown
`
	for _, test := range []struct {
		name    string
		inittab string
		openrc  bool
		want    string
	}{
		{name: "Alpine minirootfs lacks OpenRC", inittab: alpineInittab, want: "sapphire-init"},
		{name: "OpenRC present but virtual gettys unavailable", inittab: alpineInittab, openrc: true, want: "sapphire-init"},
		{name: "no inittab", want: "sapphire-init"},
		{name: "comments only", inittab: "# no configured init actions\n", want: "sapphire-init"},
		{name: "missing respawn command", inittab: "::respawn:/sbin/not-installed\n", want: "sapphire-init"},
		{name: "headless OpenRC configuration", inittab: "::sysinit:/sbin/openrc sysinit\n::wait:/sbin/openrc default\n::shutdown:/sbin/openrc shutdown\n", openrc: true, want: "/sbin/init"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			for _, path := range []string{"bin", "sbin", "etc"} {
				if err := os.MkdirAll(filepath.Join(directory, path), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(directory, "bin/busybox"), []byte("executable fixture"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"sbin/init", "sbin/getty", "sbin/reboot"} {
				if err := os.Symlink("/bin/busybox", filepath.Join(directory, path)); err != nil {
					t.Fatal(err)
				}
			}
			if test.openrc {
				if err := os.WriteFile(filepath.Join(directory, "sbin/openrc"), []byte("executable fixture"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if test.inittab != "" {
				if err := os.WriteFile(filepath.Join(directory, "etc/inittab"), []byte(test.inittab), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := detectInit(directory); got != test.want {
				t.Fatalf("init = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDetectInitPreservesInstalledDistroInit(t *testing.T) {
	for _, test := range []struct {
		name, executable, link, want string
	}{
		{"OpenRC init", "sbin/openrc-init", "", "/sbin/openrc-init"},
		{"systemd with absolute symlink", "usr/lib/systemd/systemd", "/usr/lib/systemd/systemd", "/usr/lib/systemd/systemd"},
		{"SysV init", "sbin/init", "", "/sbin/init"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			for _, path := range []string{"sbin", filepath.Dir(test.executable)} {
				if err := os.MkdirAll(filepath.Join(directory, path), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(directory, test.executable), []byte("executable fixture"), 0o755); err != nil {
				t.Fatal(err)
			}
			if test.link != "" {
				if err := os.Symlink(test.link, filepath.Join(directory, "sbin/init")); err != nil {
					t.Fatal(err)
				}
			}
			if got := detectInit(directory); got != test.want {
				t.Fatalf("init = %q, want %q", got, test.want)
			}
		})
	}
}
