package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sapphire/engine/internal/config"
	"sapphire/engine/internal/daemon"
	"sapphire/engine/internal/privatefs"
	"sapphire/engine/internal/protocol"
)

func TestMain(m *testing.M) {
	if os.Getenv("SAPPHIRE_ENGINE_CLIENT_FIXTURE") == "1" && len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(fixtureDaemon())
	}
	os.Exit(m.Run())
}

func fixtureDaemon() int {
	args := make(map[string]string)
	for i := 2; i+1 < len(os.Args); i += 2 {
		args[os.Args[i]] = os.Args[i+1]
	}
	home := args["--home"]
	lock, err := privatefs.Lock(home)
	notification := os.NewFile(3, "ready")
	if err != nil {
		json.NewEncoder(notification).Encode(protocol.Failure(err))
		notification.Close()
		return 1
	}
	defer lock.Close()
	listener, err := privatefs.Listen(home + "/control.sock")
	if err != nil {
		return 2
	}
	defer listener.Close()
	status := daemon.HostStatus{Running: true, State: "running", PID: os.Getpid(), Backend: "hypervisor", Home: home}
	status.CPUs, _ = strconv.Atoi(args["--cpus"])
	status.MemoryMiB, _ = strconv.Atoi(args["--memory-mib"])
	stop := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			json.NewEncoder(w).Encode(status)
		case "/stop":
			json.NewEncoder(w).Encode(protocol.Success(nil))
			stop <- struct{}{}
		}
	})}
	go server.Serve(listener)
	json.NewEncoder(notification).Encode(protocol.Success(nil))
	notification.Close()
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	server.Shutdown(ctx)
	return 0
}

func clientConfig(t *testing.T) config.Config {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "se-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c, _ := config.Default()
	c.Home, c.CPUs, c.MemoryMiB = dir, 2, 768
	return c
}

func TestDetachedStartReadinessAndStopWaitForLock(t *testing.T) {
	t.Setenv("SAPPHIRE_ENGINE_CLIENT_FIXTURE", "1")
	c := clientConfig(t)
	control := New(c)
	defer control.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := control.Start(ctx, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer control.Stop(ctx)
	if !status.Running || status.CPUs != 2 || status.MemoryMiB != 768 || status.PID == os.Getpid() {
		t.Fatalf("detached daemon did not acknowledge actual config: %+v", status)
	}
	again, err := control.Start(ctx, os.Args[0])
	if err != nil || again.PID != status.PID {
		t.Fatalf("duplicate daemon: %+v %v", again, err)
	}
	if err := control.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if held, err := privatefs.Held(c.Home); held || err != nil {
		t.Fatalf("stop returned before lock release: %v %v", held, err)
	}
}

func TestUnreachableLockedEngineNeverStartsOrSignalsProcess(t *testing.T) {
	c := clientConfig(t)
	lock, err := privatefs.Lock(c.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	control := New(c)
	defer control.Close()
	status, err := control.Status(context.Background())
	if err != nil || status.Running || status.State != "unavailable" {
		t.Fatalf("orphan guard status: %+v %v", status, err)
	}
	if _, err := control.Start(context.Background(), "/must-not-execute"); err == nil {
		t.Fatal("started second daemon while lifetime lock held")
	}
	if err := control.Stop(context.Background()); err == nil {
		t.Fatal("stop claimed success with no control connection")
	}
}

func TestMalformedStatusIsNotReportedAsStopped(t *testing.T) {
	c := clientConfig(t)
	listener, err := net.Listen("unix", c.Path("control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "corrupt response")
	})}
	go server.Serve(listener)
	defer server.Close()
	control := New(c)
	defer control.Close()
	if _, err := control.Status(context.Background()); err == nil {
		t.Fatal("corrupt daemon response hidden as stopped")
	}
}

func TestJoiningStartupFailsPromptlyWhenDaemonDisappears(t *testing.T) {
	for _, orphanLock := range []bool{false, true} {
		t.Run(fmt.Sprintf("lifetime-lock-%v", orphanLock), func(t *testing.T) {
			c := clientConfig(t)
			if orphanLock {
				lock, err := privatefs.Lock(c.Home)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			listener, err := net.Listen("unix", c.Path("control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			starting := make(chan struct{})
			var once sync.Once
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Connection", "close")
				json.NewEncoder(w).Encode(daemon.HostStatus{State: "starting", Backend: "hypervisor"})
				once.Do(func() { close(starting) })
			})}
			defer server.Close()
			go server.Serve(listener)
			shutdown := make(chan struct{})
			go func() {
				<-starting
				server.Shutdown(context.Background())
				close(shutdown)
			}()
			control := New(c)
			defer control.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = control.waitRunning(ctx)
			if err == nil || errors.Is(err, context.DeadlineExceeded) ||
				!strings.Contains(err.Error(), "before startup readiness") {
				t.Fatalf("dead startup consumed whole deadline: %v", err)
			}
			<-shutdown
		})
	}
}
