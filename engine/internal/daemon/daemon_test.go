package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"sapphire/engine/internal/config"
	"sapphire/engine/internal/privatefs"
	"sapphire/engine/internal/protocol"
)

func TestMain(m *testing.M) {
	if os.Getenv("SAPPHIRE_ENGINE_FIXTURE_DAEMON") == "1" && len(os.Args) > 1 && os.Args[1] == "fixture-serve" {
		var c config.Config
		if err := json.Unmarshal([]byte(os.Getenv("SAPPHIRE_ENGINE_FIXTURE_CONFIG")), &c); err != nil {
			os.Exit(2)
		}
		notification := os.NewFile(3, "ready")
		err := Run(context.Background(), c, func(err error) {
			response := protocol.Success(nil)
			if err != nil {
				response = protocol.Failure(err)
			}
			json.NewEncoder(notification).Encode(response)
			notification.Close()
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("SAPPHIRE_ENGINE_FIXTURE_DRIVER") == "1" {
		os.Exit(fixtureDriver())
	}
	os.Exit(m.Run())
}

// This is a protocol fixture, not a VM: no Hypervisor APIs or guest assets run.
func fixtureDriver() int {
	args := make(map[string]string)
	initializeDataDisk := false
	for i := 1; i < len(os.Args); {
		if os.Args[i] == "--initialize-data-disk" {
			initializeDataDisk = true
			i++
			continue
		}
		if i+1 >= len(os.Args) {
			return 2
		}
		args[os.Args[i]] = os.Args[i+1]
		i += 2
	}
	if args["--net-fd"] != "3" || args["--parent-fd"] != "5" || args["--cpus"] == "" || args["--memory-mib"] == "" {
		return 2
	}
	launches, err := os.OpenFile(args["--data-disk"]+".launches", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 2
	}
	fmt.Fprintln(launches, os.Getpid())
	launches.Close()
	// Inherited transport and guard descriptors live until process exit,
	// including the race runtime's exit delay.
	socketType, err := unix.GetsockoptInt(3, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || socketType != unix.SOCK_DGRAM {
		return 2
	}
	peer, err := unix.Getpeername(3)
	if _, ok := peer.(*unix.SockaddrUnix); err != nil || !ok {
		return 2
	}
	if _, err := unix.Write(3, make([]byte, 60)); err != nil {
		return 2
	}
	var lockInfo unix.Stat_t
	if err := unix.Fstat(4, &lockInfo); err != nil {
		return 2
	}
	bootMode := os.Getenv("SAPPHIRE_ENGINE_FIXTURE_BOOT_MODE")
	if bootMode != "" {
		console, err := os.OpenFile(args["--console"], os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return 2
		}
		fmt.Fprintln(console, "/init: line 11: mkdir: not found\nKernel panic - not syncing: fixture init died")
		console.Close()
		fmt.Fprintln(os.Stderr, "fixture VMM boot failed")
		if bootMode == "exit-before-ready" {
			return 23
		}
	}
	listener, err := net.Listen("unix", args["--vsock"])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer listener.Close()
	done := make(chan struct{})
	var once sync.Once
	quit := func() { once.Do(func() { close(done); listener.Close() }) }
	parent := os.NewFile(5, "parent-lifetime")
	go func() {
		io.Copy(io.Discard, parent)
		quit()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			break
		}
		go func() {
			defer conn.Close()
			reader := bufio.NewReader(conn)
			line, _ := reader.ReadString('\n')
			switch bootMode {
			case "exit-during-handshake":
				os.Exit(23)
			case "stall-handshake":
				<-done
				return
			case "bad-handshake":
				io.WriteString(conn, "BROKEN\n")
				return
			}
			if _, err := io.WriteString(conn, "OK\n"); err != nil {
				return
			}
			switch line {
			case "CONNECT 1024\n":
				var request protocol.Request
				if err := json.NewDecoder(reader).Decode(&request); err != nil {
					return
				}
				response := protocol.Success(request)
				if request.Action == "fixture.launch" {
					response = protocol.Success(map[string]bool{"initializeDataDisk": initializeDataDisk})
				}
				json.NewEncoder(conn).Encode(response)
				if request.Action == "shutdown" {
					delay, _ := time.ParseDuration(os.Getenv("SAPPHIRE_ENGINE_FIXTURE_SHUTDOWN_DELAY"))
					if delay > 0 {
						time.Sleep(delay)
					}
					os.WriteFile(args["--data-disk"]+".shutdown", []byte("guest completed orderly shutdown"), 0600)
				}
				if request.Action == "shutdown" || request.Action == "fixture.exit" {
					quit()
				}
			case "CONNECT 1025\n":
				request, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				request.Body.Close()
				if request.URL.Path == "/_ping" {
					io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
				} else {
					io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 16\r\nConnection: close\r\n\r\n{\"fixture\":true}")
				}
			case "CONNECT 1026\n":
				reader.ReadString('\n')
				io.Copy(conn, reader)
			}
		}()
	}
	<-done
	return 0
}

func fixtureConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv("SAPPHIRE_ENGINE_FIXTURE_DRIVER", "1")
	dir, err := os.MkdirTemp("/tmp", "sd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c, _ := config.Default()
	c.Home, c.Driver = dir, os.Args[0]
	c.CPUs, c.MemoryMiB, c.DataGiB = 2, 512, 1
	c.StartupTimeout, c.StopTimeout = 3*time.Second, 2*time.Second
	if err := os.MkdirAll(c.Path("assets"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kernel", "initrd.gz", "rootfs.img"} {
		if err := os.WriteFile(c.Path("assets/"+name), []byte("fixture; never boot"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func unixHTTP(c config.Config) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", c.Path("control.sock"))
			},
		},
	}
}

func callHTTP(t *testing.T, client *http.Client, action string) protocol.Response {
	t.Helper()
	response, err := client.Post("http://sapphire/call", "application/json", strings.NewReader(action))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result protocol.Response
	if err := protocol.Decode(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func runFixture(t *testing.T) (config.Config, *http.Client, <-chan error) {
	t.Helper()
	c := fixtureConfig(t)
	client, done := runConfiguredFixture(t, c)
	return c, client, done
}

func runConfiguredFixture(t *testing.T, c config.Config) (*http.Client, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan error, 1)
	go func() { done <- Run(ctx, c, func(err error) { ready <- err }) }()
	t.Cleanup(cancel)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("fixture daemon did not become ready")
	}
	client := unixHTTP(c)
	t.Cleanup(func() { client.CloseIdleConnections() })
	return client, done
}

func TestDaemonLifecycleAndGuestDelegation(t *testing.T) {
	c, client, done := runFixture(t)
	assertInitializationProvenance(t, client, true)
	response, err := client.Get("http://sapphire/status")
	if err != nil {
		t.Fatal(err)
	}
	var status HostStatus
	protocol.Decode(response.Body, &status)
	response.Body.Close()
	if !status.Running || !status.GuestReady || status.CPUs != 2 || status.MemoryMiB != 512 || status.Backend != "hypervisor" || status.VMMPID <= 0 {
		t.Fatalf("status: %+v", status)
	}
	if lock, err := privatefs.Lock(c.Home); !errors.Is(err, privatefs.ErrLocked) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("daemon failed to hold its lifetime lock: %v", err)
	}
	clone := callHTTP(t, client, `{"action":"machine.clone","name":"source","destination":"copy","arguments":["printf","a\nb"]}`)
	if !clone.OK || !bytes.Contains(clone.Result, []byte(`"destination":"copy"`)) {
		t.Fatalf("guest request corrupted: %+v", clone)
	}
	guestStatus := callHTTP(t, client, `{"action":"guest.status"}`)
	if !guestStatus.OK || !bytes.Contains(guestStatus.Result, []byte(`"action":"status"`)) {
		t.Fatalf("guest status routed to host: %+v", guestStatus)
	}
	unknown := callHTTP(t, client, `{"action":"unknown.action"}`)
	if unknown.OK || !strings.Contains(unknown.Error, "unsupported action") {
		t.Fatalf("unknown action faked success: %+v", unknown)
	}
	for _, name := range []string{"control.sock", "docker.sock", "vsock.sock"} {
		info, err := os.Stat(c.Path(name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode: %v %v", name, info, err)
		}
	}
	dockerTransport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.Path("docker.sock"))
	}}
	defer dockerTransport.CloseIdleConnections()
	dockerResponse, err := (&http.Client{Transport: dockerTransport, Timeout: time.Second}).Get("http://docker/version")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(dockerResponse.Body)
	dockerResponse.Body.Close()
	if string(body) != `{"fixture":true}` {
		t.Fatalf("Docker socket did not carry raw HTTP: %q", body)
	}
	response, err = client.Post("http://sapphire/stop", "application/json", nil)
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
		t.Fatal("stop did not finish")
	}
	for _, name := range []string{"control.sock", "docker.sock", "vsock.sock"} {
		if _, err := os.Lstat(c.Path(name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("left stale socket %s: %v", name, err)
		}
	}
	lock, err := privatefs.Lock(c.Home)
	if err != nil {
		t.Fatalf("lifetime lock leaked: %v", err)
	}
	lock.Close()
	info, _ := os.Stat(c.Path("data.img"))
	if info.Size() != config.GiB {
		t.Fatal("wrong sparse disk size")
	}
	if _, err := os.Stat(c.Path("data.img.shutdown")); err != nil {
		t.Fatal("engine stopped without requesting guest shutdown")
	}
}

func TestVMMExitTearsDownDaemon(t *testing.T) {
	c, client, done := runFixture(t)
	callHTTP(t, client, `{"action":"guest.fixture.exit"}`)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "VMM exited") {
			t.Fatalf("VMM death not propagated: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon survived VMM exit")
	}
	if _, err := os.Stat(c.Path("control.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("control socket survived VMM exit")
	}
}

func TestMissingAssetsFailBeforeCreatingDisk(t *testing.T) {
	c := fixtureConfig(t)
	os.Remove(c.Path("assets/kernel"))
	var notified error
	err := Run(context.Background(), c, func(err error) { notified = err })
	if err == nil || notified == nil {
		t.Fatal("missing assets accepted")
	}
	if _, err := os.Stat(c.Path("data.img")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created disk despite missing assets")
	}
}

func TestDriverArgumentsMatchNativeContract(t *testing.T) {
	c := fixtureConfig(t)
	args := driverArguments(c, false)
	values := make(map[string]string)
	for i := 0; i < len(args); i += 2 {
		values[args[i]] = args[i+1]
	}
	if values["--net-fd"] != "3" || values["--parent-fd"] != "5" || values["--cpus"] != strconv.Itoa(c.CPUs) ||
		values["--memory-mib"] != strconv.Itoa(c.MemoryMiB) ||
		values["--console"] != filepath.Join(c.Home, "console.log") ||
		values["--root-disk"] != c.Path("assets/rootfs.img") {
		t.Fatalf("native arguments: %v", args)
	}
}

func TestDaemonCrashReleasesVMMGuardAndAllowsRestart(t *testing.T) {
	c := fixtureConfig(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cJSON, _ := json.Marshal(c)
	command := exec.Command(os.Args[0], "fixture-serve")
	command.Env = append(os.Environ(), "SAPPHIRE_ENGINE_FIXTURE_DAEMON=1", "SAPPHIRE_ENGINE_FIXTURE_CONFIG="+string(cJSON))
	command.ExtraFiles = []*os.File{writer}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	writer.Close()
	reader.SetReadDeadline(time.Now().Add(10 * time.Second))
	var readiness protocol.Response
	if err := protocol.Decode(reader, &readiness); err != nil || !readiness.OK {
		command.Process.Kill()
		command.Wait()
		t.Fatalf("fixture daemon readiness: %+v %v stderr %s", readiness, err, &stderr)
	}
	if held, err := privatefs.Held(c.Home); !held || err != nil {
		t.Fatalf("VMM did not inherit its lifetime guard: %v %v", held, err)
	}
	disk, err := os.OpenFile(c.Path("data.img"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("fixture data survives daemon crash and restart")
	_, err = disk.WriteAt(marker, 4096)
	disk.Close()
	if err != nil {
		t.Fatal(err)
	}
	// This is the test's own exec.Cmd, never a PID obtained from external state.
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	command.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		held, err := privatefs.Held(c.Home)
		if err != nil {
			t.Fatal(err)
		}
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan VMM retained disk guard after parent EOF; stderr %s", &stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Reuse the same disks and stale sockets after the fixture child's exit.
	client, done := runConfiguredFixture(t, c)
	assertInitializationProvenance(t, client, false)
	disk, err = os.Open(c.Path("data.img"))
	if err != nil {
		t.Fatal(err)
	}
	persisted := make([]byte, len(marker))
	_, err = disk.ReadAt(persisted, 4096)
	disk.Close()
	if err != nil || !bytes.Equal(persisted, marker) {
		t.Fatalf("restart changed existing disk bytes: %q %v", persisted, err)
	}
	response, err := client.Post("http://sapphire/stop", "application/json", nil)
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
		t.Fatal("restarted daemon did not stop")
	}
}
