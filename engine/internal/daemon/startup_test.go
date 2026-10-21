package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"sapphire/engine/internal/privatefs"
	"sapphire/engine/internal/protocol"
)

func TestFailedStartupExitsOnceAndCanExplicitlyRestart(t *testing.T) {
	for _, mode := range []string{"exit-before-ready", "exit-during-handshake", "stall-handshake", "bad-handshake"} {
		t.Run(mode, func(t *testing.T) {
			c := fixtureConfig(t)
			t.Setenv("SAPPHIRE_ENGINE_FIXTURE_BOOT_MODE", mode)
			c.StartupTimeout, c.StopTimeout = 30*time.Second, 45*time.Second
			if mode == "stall-handshake" || mode == "bad-handshake" {
				c.StartupTimeout = time.Second
			}
			// An old boot failure must not be attributed to this launch.
			if err := os.WriteFile(c.Path("console.log"), []byte("stale boot failure\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready, done := make(chan error, 2), make(chan error, 1)
			go func() { done <- Run(ctx, c, func(err error) { ready <- err }) }()
			var notified error
			select {
			case notified = <-ready:
			case <-time.After(6 * time.Second):
				cancel()
				t.Fatal("failed startup waited for readiness or orderly shutdown grace")
			}
			if notified == nil {
				t.Fatal("failed VMM acknowledged successful startup")
			}
			message := notified.Error()
			for _, want := range []string{"mkdir: not found", "Kernel panic", "fixture VMM boot failed", "console.log", "vmm.log"} {
				if !strings.Contains(message, want) {
					t.Fatalf("missing startup diagnostic %q: %v", want, notified)
				}
			}
			if strings.Contains(message, "stale boot failure") {
				t.Fatalf("reported previous launch's console: %v", notified)
			}
			if strings.HasPrefix(mode, "exit-") {
				if !strings.Contains(message, "VMM exited during startup: exit status 23") {
					t.Fatalf("VMM exit lost behind a protocol error: %v", notified)
				}
			} else if !errors.Is(notified, context.DeadlineExceeded) {
				t.Fatalf("readiness timeout lost: %v", notified)
			}
			select {
			case err := <-done:
				if err == nil || err.Error() != message {
					t.Fatalf("startup result differs from readiness: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("failed startup teardown did not finish")
			}
			select {
			case err := <-ready:
				t.Fatalf("readiness notified more than once: %v", err)
			default:
			}
			assertLaunchCount(t, c.Path("data.img.launches"), 1)
			if held, err := privatefs.Held(c.Home); err != nil || held {
				t.Fatalf("failed boot retained its disk lock: %v %v", held, err)
			}
			for _, name := range []string{"control.sock", "docker.sock", "vsock.sock"} {
				if _, err := os.Lstat(c.Path(name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed startup left socket %s: %v", name, err)
				}
			}
			t.Setenv("SAPPHIRE_ENGINE_FIXTURE_BOOT_MODE", "")
			c.StartupTimeout = 3 * time.Second
			control, restarted := runConfiguredFixture(t, c)
			assertLaunchCount(t, c.Path("data.img.launches"), 2)
			assertInitializationProvenance(t, control, false)
			response, err := control.Post("http://sapphire/stop", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			select {
			case err := <-restarted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("explicitly restarted fixture did not stop")
			}
		})
	}
}

func assertLaunchCount(t *testing.T, path string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if count := len(strings.Fields(string(data))); count != want {
		t.Fatalf("expected %d VMM launches, got %d: %s", want, count, data)
	}
}

func TestDetachedFailureReadinessIncludesVMMExitAndConsole(t *testing.T) {
	c := fixtureConfig(t)
	t.Setenv("SAPPHIRE_ENGINE_FIXTURE_BOOT_MODE", "exit-during-handshake")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cJSON, _ := json.Marshal(c)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "fixture-serve")
	command.Env = append(os.Environ(), "SAPPHIRE_ENGINE_FIXTURE_DAEMON=1", "SAPPHIRE_ENGINE_FIXTURE_CONFIG="+string(cJSON))
	command.ExtraFiles = []*os.File{writer}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	reader.SetReadDeadline(time.Now().Add(5 * time.Second))
	var readiness protocol.Response
	decodeErr := protocol.Decode(reader, &readiness)
	waitErr := command.Wait()
	if decodeErr != nil || waitErr == nil || readiness.OK ||
		!strings.Contains(readiness.Error, "VMM exited during startup") ||
		!strings.Contains(readiness.Error, "mkdir: not found") {
		t.Fatalf("detached failure: response %+v, decode %v, wait %v, stderr %s",
			readiness, decodeErr, waitErr, &stderr)
	}
	assertLaunchCount(t, c.Path("data.img.launches"), 1)
}

func TestOrderlyShutdownWaitsForGuestCompletionAfterACK(t *testing.T) {
	t.Setenv("SAPPHIRE_ENGINE_FIXTURE_SHUTDOWN_DELAY", "250ms")
	c, control, done := runFixture(t)
	response, err := control.Post("http://sapphire/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("orderly stop did not complete")
	}
	if _, err := os.Stat(c.Path("data.img.shutdown")); err != nil {
		t.Fatal("VMM killed before guest finished shutdown after its ACK")
	}
}

func TestTransportFailurePrefersConcurrentVMMExit(t *testing.T) {
	process := &child{done: make(chan struct{})}
	transport := errors.New("Ethernet endpoint disconnected")
	exit := errors.New("exit status 23")
	go func() {
		time.Sleep(20 * time.Millisecond)
		process.waitErr = exit
		close(process.done)
	}()
	err := process.transportError(transport, " during startup")
	if !errors.Is(err, exit) || !errors.Is(err, transport) ||
		!strings.HasPrefix(err.Error(), "VMM exited during startup") {
		t.Fatalf("transport failure obscured concurrent process exit: %v", err)
	}
}
