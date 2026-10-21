package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"sapphire/engine/internal/config"
	"sapphire/engine/internal/forward"
	"sapphire/engine/internal/network"
	"sapphire/engine/internal/privatefs"
	"sapphire/engine/internal/protocol"
	"sapphire/engine/internal/proxy"
	"sapphire/engine/internal/vsock"
)

type HostStatus struct {
	Running      bool           `json:"running"`
	PID          int            `json:"pid"`
	CPUs         int            `json:"cpus"`
	MemoryMiB    int            `json:"memoryMiB"`
	Backend      string         `json:"backend"`
	State        string         `json:"state"`
	VMMPID       int            `json:"vmmPID,omitempty"`
	GuestReady   bool           `json:"guestReady"`
	Home         string         `json:"home"`
	DockerSocket string         `json:"dockerSocket"`
	DataGiB      int64          `json:"dataGiB"`
	Subnet       string         `json:"subnet"`
	MAC          string         `json:"mac"`
	StartedAt    time.Time      `json:"startedAt,omitempty"`
	Forwards     []forward.Spec `json:"forwards"`
	Warnings     []string       `json:"warnings"`
}

func Stopped(c config.Config) HostStatus {
	return HostStatus{
		Backend: "hypervisor", State: "stopped", Home: c.Home,
		DockerSocket: c.Path("docker.sock"), CPUs: c.CPUs, MemoryMiB: c.MemoryMiB,
		DataGiB: c.DataGiB, Subnet: c.Subnet, MAC: c.MAC,
		Forwards: []forward.Spec{}, Warnings: []string{},
	}
}

type Daemon struct {
	config   config.Config
	ctx      context.Context
	guest    vsock.Client
	forwards *forward.Manager
	docker   *forward.DockerWatch
	child    *child
	mu       sync.Mutex
	actions  sync.Mutex
	status   HostStatus
	stopOnce sync.Once
	stop     chan struct{}
}

// Run owns one lock, one VMM, and one network stack for its whole lifetime.
// ready is called exactly once, after guest control readiness or a startup error.
func Run(ctx context.Context, c config.Config, ready func(error)) (runErr error) {
	var readyOnce sync.Once
	notify := func(err error) {
		readyOnce.Do(func() {
			if ready != nil {
				ready(err)
			}
		})
	}
	defer func() { notify(runErr) }()
	if err := c.Validate(); err != nil {
		return err
	}
	if err := privatefs.Directory(c.Home); err != nil {
		return err
	}
	lock, err := privatefs.Lock(c.Home)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Every socket removal, including stale recovery, stays inside this lock.
	for _, name := range []string{"control.sock", "docker.sock", "vsock.sock"} {
		if err := privatefs.RemoveSocket(c.Path(name)); err != nil {
			return err
		}
	}
	defer func() {
		for _, name := range []string{"control.sock", "docker.sock", "vsock.sock"} {
			if err := privatefs.RemoveSocket(c.Path(name)); err != nil {
				log.Print(err)
			}
		}
	}()
	initializeDataDisk, err := prepare(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d := &Daemon{config: c, ctx: ctx, guest: vsock.Client{Path: c.Path("vsock.sock")}, stop: make(chan struct{})}
	d.status = Stopped(c)
	d.status.PID, d.status.State, d.status.StartedAt = os.Getpid(), "starting", time.Now().UTC()
	d.forwards = forward.New(ctx, d.guest.DialTCP)
	defer d.forwards.Close()
	d.docker = forward.NewDockerWatch(d.guest, d.forwards, d.warning)
	defer d.docker.Stop()
	control, err := privatefs.Listen(c.Path("control.sock"))
	if err != nil {
		return err
	}
	defer control.Close()
	docker, err := privatefs.Listen(c.Path("docker.sock"))
	if err != nil {
		return err
	}
	defer docker.Close()
	host, ethernet, err := network.SocketPair()
	if err != nil {
		return err
	}
	defer host.Close()
	defer ethernet.Close()
	stack, err := network.New(c, host)
	if err != nil {
		return err
	}
	defer stack.Close()
	d.child, err = startDriver(c, ethernet, lock, initializeDataDisk)
	ethernet.Close()
	if err != nil {
		return err
	}
	var stopGrace time.Duration
	defer func() {
		if err := d.child.stop(stopGrace); err != nil {
			runErr = errors.Join(runErr, err)
		}
		cancel()
		if runErr != nil {
			runErr = d.child.diagnosticError(runErr)
		}
	}()
	d.status.VMMPID = d.child.cmd.Process.Pid
	failures := make(chan error, 3)
	go func() {
		if err := stack.Run(ctx); ctx.Err() == nil {
			failures <- fmt.Errorf("Ethernet transport exited: %w", err)
		}
	}()
	go func() {
		err := proxy.Serve(ctx, docker, func(ctx context.Context) (net.Conn, error) {
			return d.guest.Dial(ctx, vsock.DockerPort)
		})
		if ctx.Err() == nil {
			failures <- fmt.Errorf("Docker proxy exited: %w", err)
		}
	}()
	server := &http.Server{
		Handler: d.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := server.Serve(control); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failures <- err
		}
	}()
	defer func() {
		server.Close()
	}()
	startup, cancelStartup := context.WithTimeout(ctx, c.StartupTimeout)
	defer cancelStartup()
	started := make(chan error, 1)
	go func() { started <- d.waitReady(startup) }()
	select {
	case err := <-started:
		select {
		case <-d.child.done:
			return d.child.exitError(" during startup")
		default:
		}
		if err != nil {
			return fmt.Errorf("guest control not ready: %w", err)
		}
		d.mu.Lock()
		d.status.State, d.status.Running, d.status.GuestReady = "running", true, true
		d.mu.Unlock()
		notify(nil)
		go d.docker.TryStart(ctx)
	case <-d.child.done:
		return d.child.exitError(" during startup")
	case err := <-failures:
		return d.child.transportError(err, " during startup")
	case <-ctx.Done():
		return ctx.Err()
	case <-d.stop:
		return errors.New("engine stopped during startup")
	}
	select {
	case <-ctx.Done():
	case <-d.stop:
	case <-d.child.done:
		return d.child.exitError("")
	case err := <-failures:
		return d.child.transportError(err, "")
	}
	d.mu.Lock()
	d.status.State, d.status.Running, d.status.GuestReady = "stopping", false, false
	d.mu.Unlock()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), min(3*time.Second, c.StopTimeout))
	defer shutdownCancel()
	if response, err := d.guest.Call(shutdownCtx, protocol.Request{Action: "shutdown"}); err != nil {
		d.warning(fmt.Errorf("guest shutdown: %w", err))
	} else if err := response.Err(); err != nil {
		d.warning(err)
	}
	stopGrace = c.StopTimeout
	return nil
}

func prepare(c config.Config) (initializeDataDisk bool, err error) {
	if err := privatefs.Directory(c.Path("assets")); err != nil {
		return false, err
	}
	for _, name := range []string{"kernel", "initrd.gz", "rootfs.img"} {
		path := c.Path("assets/" + name)
		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("guest asset %s: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return false, fmt.Errorf("guest asset %s must be a nonempty regular file", path)
		}
	}
	// Fail before creating a large disk when the native driver is unavailable.
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	if _, err := c.ResolveDriver(executable); err != nil {
		return false, err
	}
	created, err := privatefs.EnsureDataDisk(c.Path("data.img"), c.DataGiB*config.GiB)
	if err != nil {
		return false, err
	}
	console, err := privatefs.Open(c.Path("console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return false, err
	}
	if err := console.Close(); err != nil {
		return false, err
	}
	return created, nil
}

func (d *Daemon) waitReady(ctx context.Context) error {
	delay := 100 * time.Millisecond
	var last error
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		response, err := d.guest.Call(attempt, protocol.Request{Action: "status"})
		cancel()
		if err == nil {
			err = response.Err()
		}
		if err == nil {
			if err := os.Chmod(d.config.Path("vsock.sock"), 0600); err != nil {
				return err
			}
			return nil
		}
		last = err
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w; last attempt: %v", ctx.Err(), last)
		case <-timer.C:
			delay = min(delay*2, time.Second)
		}
	}
}

func (d *Daemon) warning(err error) {
	if err == nil {
		return
	}
	log.Print(err)
	d.mu.Lock()
	defer d.mu.Unlock()
	message := err.Error()
	if count := len(d.status.Warnings); count > 0 && d.status.Warnings[count-1] == message {
		return
	}
	d.status.Warnings = append(d.status.Warnings, message)
	if len(d.status.Warnings) > 8 {
		d.status.Warnings = d.status.Warnings[len(d.status.Warnings)-8:]
	}
}

func (d *Daemon) Status() HostStatus {
	d.mu.Lock()
	status := d.status
	status.Warnings = append([]string{}, status.Warnings...)
	d.mu.Unlock()
	select {
	case <-d.child.done:
		status.Running, status.GuestReady, status.State = false, false, "stopping"
	default:
	}
	status.Forwards = d.forwards.List()
	return status
}

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.Status())
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, protocol.Success(d.Status()))
		http.NewResponseController(w).Flush()
		d.stopOnce.Do(func() { close(d.stop) })
	})
	mux.HandleFunc("POST /call", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxMessage)
		var request protocol.Request
		if err := protocol.Decode(r.Body, &request); err != nil {
			writeJSON(w, protocol.Failure(err))
			return
		}
		if err := request.Validate(); err != nil {
			writeJSON(w, protocol.Failure(err))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		response := d.call(ctx, request)
		writeJSON(w, response)
		if request.Action == "host.stop" && response.OK {
			http.NewResponseController(w).Flush()
			d.stopOnce.Do(func() { close(d.stop) })
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Print(err)
	}
}
