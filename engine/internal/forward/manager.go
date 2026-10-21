package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"

	"sapphire/engine/internal/proxy"
	"sapphire/engine/internal/vsock"
)

type Spec struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	HostPort int    `json:"hostPort"`
	Source   string `json:"source,omitempty"`
}

func (s Spec) target() vsock.Target { return vsock.Target{Host: s.Host, Port: s.Port} }

type entry struct {
	spec   Spec
	cancel context.CancelFunc
	done   chan struct{}
}

type Manager struct {
	ctx     context.Context
	dial    func(context.Context, vsock.Target) (net.Conn, error)
	mu      sync.Mutex
	entries map[int]*entry
	closed  bool
}

func New(ctx context.Context, dial func(context.Context, vsock.Target) (net.Conn, error)) *Manager {
	return &Manager{ctx: ctx, dial: dial, entries: make(map[int]*entry)}
}

func (m *Manager) Add(spec Spec) (Spec, error) {
	if err := spec.target().Validate(); err != nil {
		return Spec{}, err
	}
	if spec.HostPort < 0 || spec.HostPort > 65535 {
		return Spec{}, errors.New("hostPort must be between 0 and 65535")
	}
	if spec.Source == "" {
		spec.Source = "manual"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return Spec{}, errors.New("forwarding is closed")
	}
	if existing, ok := m.entries[spec.HostPort]; ok {
		if existing.spec == spec {
			return spec, nil
		}
		return Spec{}, fmt.Errorf("port %d is already forwarded by %s", spec.HostPort, existing.spec.Source)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.HostPort)))
	if err != nil {
		return Spec{}, err
	}
	spec.HostPort = listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(m.ctx)
	e := &entry{spec: spec, cancel: cancel, done: make(chan struct{})}
	m.entries[spec.HostPort] = e
	go func() {
		defer close(e.done)
		proxy.Serve(ctx, listener, func(ctx context.Context) (net.Conn, error) {
			return m.dial(ctx, spec.target())
		})
		listener.Close()
	}()
	return spec, nil
}

func (m *Manager) Remove(port int, source string) error {
	m.mu.Lock()
	e, ok := m.entries[port]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	if e.spec.Source != source {
		m.mu.Unlock()
		return fmt.Errorf("port %d belongs to %s", port, e.spec.Source)
	}
	delete(m.entries, port)
	e.cancel()
	m.mu.Unlock()
	<-e.done
	return nil
}

func (m *Manager) List() []Spec {
	m.mu.Lock()
	defer m.mu.Unlock()
	specs := make([]Spec, 0, len(m.entries))
	for _, e := range m.entries {
		specs = append(specs, e.spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].HostPort < specs[j].HostPort })
	return specs
}

// Sync changes only this source's listeners; conflicts remain visible to callers.
func (m *Manager) Sync(source string, wanted []Spec) error {
	desired := make(map[int]Spec, len(wanted))
	for _, spec := range wanted {
		spec.Source = source
		desired[spec.HostPort] = spec
	}
	var failures []error
	for _, current := range m.List() {
		if current.Source != source {
			continue
		}
		if spec, ok := desired[current.HostPort]; !ok || spec != current {
			failures = append(failures, m.Remove(current.HostPort, source))
		}
	}
	for _, spec := range desired {
		_, err := m.Add(spec)
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	entries := m.entries
	m.entries = make(map[int]*entry)
	for _, e := range entries {
		e.cancel()
	}
	m.mu.Unlock()
	for _, e := range entries {
		<-e.done
	}
}
