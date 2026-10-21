package guest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

type agent struct {
	supervisor  *supervisor
	machines    *machines
	services    *services
	bootID      string
	cgroupError error
	shutdown    chan struct{}
	requestMu   sync.Mutex
	stopping    bool
	active      sync.WaitGroup
}

func (a *agent) handle(ctx context.Context, req Request) (any, error) {
	machineAction := strings.HasPrefix(req.Action, "machine.")
	if !machineAction && (req.Name != "" || req.Configuration != nil || req.Destination != "") {
		return nil, errors.New("name/configuration/destination are only valid for machine actions")
	}
	if req.Action != "machine.exec" && len(req.Arguments) > 0 {
		return nil, errors.New("arguments are only valid for machine.exec")
	}
	if req.Action != "machine.clone" && req.Destination != "" {
		return nil, errors.New("destination is only valid for machine.clone")
	}
	if req.Action != "machine.create" && req.Configuration != nil {
		return nil, errors.New("configuration is only valid for machine.create")
	}
	if req.Action == "machine.list" && req.Name != "" {
		return nil, errors.New("machine.list takes no name")
	}
	mutates := machineAction && req.Action != "machine.list" || req.Action == "docker.start" || req.Action == "k8s.start"
	if mutates && a.cgroupError != nil {
		return nil, fmt.Errorf("cgroup v2 unavailable: %w", a.cgroupError)
	}
	switch req.Action {
	case "shutdown":
		return map[string]bool{"accepted": true}, nil
	case "status":
		return a.status(ctx), nil
	case "metrics":
		return a.metrics(), nil
	case "machine.list":
		return a.machines.list()
	case "machine.create":
		if req.Configuration == nil {
			return nil, errors.New("machine.create requires configuration")
		}
		config := *req.Configuration
		if req.Name != "" {
			if config.Name != "" && config.Name != req.Name {
				return nil, errors.New("request name and configuration.name differ")
			}
			config.Name = req.Name
		}
		return a.machines.create(ctx, config)
	case "machine.start":
		return a.machines.start(ctx, req.Name)
	case "machine.stop":
		return a.machines.stop(ctx, req.Name)
	case "machine.delete":
		return nil, a.machines.delete(ctx, req.Name)
	case "machine.clone":
		return a.machines.clone(ctx, req.Name, req.Destination)
	case "machine.exec":
		return a.machines.exec(ctx, req.Name, req.Arguments)
	case "docker.start":
		return a.services.startDocker(ctx)
	case "docker.stop":
		return a.services.stopDocker(ctx)
	case "docker.status":
		return a.services.dockerStatus(ctx), nil
	case "k8s.start":
		return a.services.startK3s(ctx)
	case "k8s.stop":
		return a.services.stopK3s(ctx)
	case "k8s.status":
		return a.services.k3sStatus(ctx), nil
	case "k8s.kubeconfig":
		return a.services.kubeconfig(ctx)
	default:
		return nil, fmt.Errorf("unsupported action %q", req.Action)
	}
}

func requestTimeout(action string) time.Duration {
	switch action {
	case "machine.create", "machine.clone":
		return 4*time.Minute + 45*time.Second
	case "machine.exec":
		return 10 * time.Minute
	case "k8s.start":
		return 3 * time.Minute
	case "docker.start":
		return time.Minute
	case "k8s.stop", "docker.stop":
		return time.Minute
	default:
		return 30 * time.Second
	}
}

func (a *agent) serveRPC(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	a.requestMu.Lock()
	if a.stopping {
		a.requestMu.Unlock()
		return
	}
	a.active.Add(1)
	a.requestMu.Unlock()
	defer a.active.Done()
	closeOnShutdown := context.AfterFunc(ctx, func() { conn.Close() })
	defer closeOnShutdown()
	reader := bufio.NewScanner(conn)
	reader.Buffer(make([]byte, 4096), MaxRequestSize+1)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if !reader.Scan() {
		if err := reader.Err(); err != nil {
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = writeResponse(conn, Response{Error: err.Error()})
		}
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	req, err := DecodeRequest(reader.Bytes())
	var result any
	if err == nil {
		requestContext, cancel := context.WithTimeout(ctx, requestTimeout(req.Action))
		defer cancel()
		cancelOnDisconnect(conn, cancel)
		result, err = a.handle(requestContext, req)
	}
	response := Response{OK: err == nil, Result: result}
	if err != nil {
		response.Error = err.Error()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := writeResponse(conn, response); err != nil {
		return
	}
	if req.Action == "shutdown" && response.OK {
		select {
		case a.shutdown <- struct{}{}:
		default:
		}
	}
}

func cancelOnDisconnect(conn net.Conn, cancel context.CancelFunc) {
	go func() {
		var byte [1]byte
		// One request per connection. EOF, read failure or extra request data
		// ends this call; a peer must keep its write side open until the reply.
		_, _ = conn.Read(byte[:])
		cancel()
	}()
}

func writeResponse(writer io.Writer, response Response) error {
	data, err := json.Marshal(response)
	if err != nil {
		data, _ = json.Marshal(Response{Error: "result could not be encoded as JSON"})
	}
	if len(data)+1 > MaxResponseSize {
		data, _ = json.Marshal(Response{Error: "result exceeds the 4 MiB RPC response limit"})
	}
	data = append(data, '\n')
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (a *agent) listen(ctx context.Context) error {
	type endpoint struct {
		port    uint32
		limit   int
		handler func(context.Context, net.Conn)
	}
	endpoints := []endpoint{
		{RPCPort, 32, a.serveRPC},
		{DockerPort, 128, proxyDocker},
		{ForwardPort, 128, proxyTCP},
	}
	var listeners []*vsock.Listener
	for _, endpoint := range endpoints {
		listener, err := vsock.Listen(endpoint.port, nil)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			return fmt.Errorf("listen vsock port %d: %w", endpoint.port, err)
		}
		listeners = append(listeners, listener)
		go acceptConnections(ctx, listener, endpoint.limit, endpoint.handler)
	}
	go func() {
		<-ctx.Done()
		for _, listener := range listeners {
			listener.Close()
		}
	}()
	log.Printf("ready: vsock RPC %d, Docker %d, TCP %d (host CID only)", RPCPort, DockerPort, ForwardPort)
	return nil
}

func acceptConnections(ctx context.Context, listener net.Listener, maximum int, handler func(context.Context, net.Conn)) {
	active := make(chan struct{}, maximum)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("vsock accept: %v", err)
			}
			return
		}
		address, ok := conn.RemoteAddr().(*vsock.Addr)
		if !ok || address.ContextID != vsock.Host {
			conn.Close()
			continue
		}
		select {
		case active <- struct{}{}:
			go func() {
				defer func() { <-active }()
				handler(ctx, conn)
			}()
		default:
			conn.Close()
		}
	}
}

func proxyDocker(ctx context.Context, client net.Conn) {
	defer client.Close()
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", "/var/run/docker.sock")
	if err != nil {
		// Docker expects HTTP on this port, including failures before startup.
		message := "Sapphire Docker engine is stopped; call docker.start\n"
		_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(client, "HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(message), message)
		return
	}
	defer upstream.Close()
	bridge(ctx, client, client, upstream)
}

type forwardRequest struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func proxyTCP(ctx context.Context, client net.Conn) {
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReaderSize(client, 4096)
	line, err := reader.ReadSlice('\n')
	var request forwardRequest
	if err == nil {
		decoder := json.NewDecoder(strings.NewReader(string(line)))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&request)
		if err == nil && decoder.Decode(new(any)) != io.EOF {
			err = errors.New("expected one forwarding JSON object")
		}
	}
	if request.Host == "" {
		request.Host = "127.0.0.1"
	}
	if err == nil && (request.Port < 1 || request.Port > 65535 || strings.ContainsAny(request.Host, "\x00\r\n/") || len(request.Host) > 253) {
		err = errors.New("invalid forwarding host or port")
	}
	var upstream net.Conn
	if err == nil {
		upstream, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(request.Host, strconv.Itoa(request.Port)))
	}
	if err != nil {
		_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = json.NewEncoder(client).Encode(Response{Error: err.Error()})
		return
	}
	defer upstream.Close()
	_ = client.SetReadDeadline(time.Time{})
	// reader retains bytes received in the same packet after the JSON newline.
	bridge(ctx, client, reader, upstream)
}

func bridge(ctx context.Context, client net.Conn, source io.Reader, upstream net.Conn) {
	stop := context.AfterFunc(ctx, func() { client.Close(); upstream.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, source)
		if half, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			upstream.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if half, ok := client.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			client.Close()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func readText(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}

func (a *agent) metrics() map[string]any {
	memory := ParseMeminfo(readText("/proc/meminfo"))
	load := strings.Fields(readText("/proc/loadavg"))
	cpu := readText("/proc/stat")
	return map[string]any{
		"cpuCount": runtime.NumCPU(), "memoryTotalBytes": memory["MemTotal"],
		"memoryAvailableBytes": memory["MemAvailable"], "memoryFreeBytes": memory["MemFree"],
		"swapTotalBytes": memory["SwapTotal"], "swapFreeBytes": memory["SwapFree"],
		"uptime": readText("/proc/uptime"), "loadAverage": load, "cpuStat": cpu,
		"interrupts": readText("/proc/interrupts"), "softirqs": readText("/proc/softirqs"),
		"memoryPressure": readText("/proc/pressure/memory"), "cpuPressure": readText("/proc/pressure/cpu"),
		"machines": a.machineMetrics(),
	}
}

func (a *agent) machineMetrics() map[string]any {
	metrics := make(map[string]any)
	infos, _ := a.machines.list()
	for _, info := range infos {
		if info.Status != "running" {
			continue
		}
		base := machineCgroup(info.Name)
		metrics[info.Name] = map[string]string{
			"memoryCurrent": readText(base + "/memory.current"), "memoryMax": readText(base + "/memory.max"),
			"memoryEvents": readText(base + "/memory.events"), "cpuStat": readText(base + "/cpu.stat"),
			"cpuMax": readText(base + "/cpu.max"), "pidsCurrent": readText(base + "/pids.current"),
			"pidsMax": readText(base + "/pids.max"),
		}
	}
	return metrics
}

func (a *agent) status(ctx context.Context) map[string]any {
	var uname unix.Utsname
	_ = unix.Uname(&uname)
	kernel := unix.ByteSliceToString(uname.Release[:])
	interfaces, _ := net.Interfaces()
	addresses := make(map[string][]string)
	for _, iface := range interfaces {
		addrs, _ := iface.Addrs()
		for _, address := range addrs {
			addresses[iface.Name] = append(addresses[iface.Name], address.String())
		}
	}
	warnings := []string{}
	clockError := ValidateWallClock(time.Now())
	if clockError != nil {
		warnings = append(warnings, clockError.Error())
	}
	if a.cgroupError != nil {
		warnings = append(warnings, a.cgroupError.Error())
	}
	infos, err := a.machines.list()
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	return map[string]any{
		"version": Version, "protocol": Protocol, "bootID": a.bootID, "pid": os.Getpid(),
		"kernel": kernel, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(),
		"wallClock": time.Now().UTC().Format(time.RFC3339), "clockValid": clockError == nil,
		"rtc":    readText("/sys/class/rtc/rtc0/name"),
		"memory": ParseMeminfo(readText("/proc/meminfo")), "network": addresses,
		"docker": a.services.dockerStatus(ctx), "kubernetes": a.services.k3sStatus(ctx),
		"machines": infos, "warnings": warnings, "cgroupV2": a.cgroupError == nil,
		"capabilities": map[string]any{
			"distributions":  []string{"alpine", "ubuntu", "debian", "fedora"},
			"machineNetwork": "shared-guest", "rosetta": false, "shareHome": false,
			"machineExecPTY": false, "machineInit": "distro init when present, sapphire-init otherwise",
			"machineDisk": "sparse ext4", "pidsMax": 4096,
		},
	}
}
