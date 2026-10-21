package forward

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"sapphire/engine/internal/vsock"
)

// DockerWatch uses one blocking event stream, plus an initial snapshot. A lost
// stream is reconnected with bounded backoff, never by periodically listing.
type DockerWatch struct {
	client                  *http.Client
	transport               *http.Transport
	manager                 *Manager
	report                  func(error)
	mu                      sync.Mutex
	cancel                  context.CancelFunc
	done                    chan struct{}
	generation              uint64
	unsupportedPublications atomic.Int64
}

func NewDockerWatch(client vsock.Client, manager *Manager, report func(error)) *DockerWatch {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return client.Dial(ctx, vsock.DockerPort)
		},
		MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 0,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	return &DockerWatch{
		client: &http.Client{Transport: transport}, transport: transport,
		manager: manager, report: report,
	}
}

func (w *DockerWatch) TryStart(ctx context.Context) {
	w.mu.Lock()
	generation := w.generation
	w.mu.Unlock()
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := w.get(probe, "/_ping")
	if err == nil {
		response.Body.Close()
		w.mu.Lock()
		if w.generation == generation {
			w.start(ctx)
		}
		w.mu.Unlock()
	}
}

func (w *DockerWatch) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.start(ctx)
}

func (w *DockerWatch) start(ctx context.Context) {
	if w.cancel != nil || ctx.Err() != nil {
		return
	}
	ctx, w.cancel = context.WithCancel(ctx)
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		var backoff time.Duration
		for ctx.Err() == nil {
			duration, err := w.session(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				w.report(err)
			}
			backoff = dockerRetryDelay(backoff, duration)
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func dockerRetryDelay(previous, sessionDuration time.Duration) time.Duration {
	if previous == 0 || sessionDuration >= 30*time.Second {
		return time.Second
	}
	return min(previous*2, 30*time.Second)
}

func (w *DockerWatch) Stop() {
	w.mu.Lock()
	w.generation++
	if w.cancel == nil {
		w.mu.Unlock()
		return
	}
	w.cancel()
	<-w.done
	w.cancel = nil
	w.unsupportedPublications.Store(0)
	w.transport.CloseIdleConnections()
	w.manager.Sync("docker", nil)
	w.mu.Unlock()
}

func (w *DockerWatch) get(ctx context.Context, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := w.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("Docker %s returned HTTP %d", path, response.StatusCode)
	}
	return response, nil
}

func (w *DockerWatch) session(ctx context.Context) (time.Duration, error) {
	filters := url.QueryEscape(`{"type":["container"]}`)
	events, err := w.get(ctx, "/events?filters="+filters)
	if err != nil {
		return 0, err
	}
	defer events.Body.Close()
	// Subscribe before listing so a container change during the snapshot is kept.
	if err := w.refresh(ctx); err != nil {
		return 0, err
	}
	started := time.Now()
	decoder := json.NewDecoder(events.Body)
	for {
		var event struct {
			Action string `json:"Action"`
			Status string `json:"status"`
		}
		if err := decoder.Decode(&event); err != nil {
			return time.Since(started), err
		}
		action := event.Action
		if action == "" {
			action = event.Status
		}
		switch action {
		case "start", "stop", "die", "destroy", "restart", "update", "unpause":
			if err := w.refresh(ctx); err != nil {
				return time.Since(started), err
			}
		}
	}
}

type container struct {
	Ports []struct {
		IP         string `json:"IP"`
		PublicPort int    `json:"PublicPort"`
		Type       string `json:"Type"`
	} `json:"Ports"`
}

func (w *DockerWatch) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := w.get(ctx, "/containers/json")
	if err != nil {
		return err
	}
	var containers []container
	err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&containers)
	response.Body.Close()
	if err != nil {
		return err
	}
	wanted := make(map[int]Spec)
	var unsupported int64
	for _, c := range containers {
		for _, port := range c.Ports {
			if port.PublicPort < 1 || port.PublicPort > 65535 {
				continue
			}
			if port.Type != "tcp" {
				unsupported++
				continue
			}
			host := port.IP
			switch host {
			case "", "0.0.0.0", "::":
				host = "127.0.0.1"
			}
			if net.ParseIP(host) == nil {
				continue
			}
			wanted[port.PublicPort] = Spec{Host: host, Port: port.PublicPort, HostPort: port.PublicPort, Source: "docker"}
		}
	}
	if previous := w.unsupportedPublications.Swap(unsupported); unsupported > 0 && previous != unsupported {
		w.report(fmt.Errorf("UDP and other non-TCP host publications are unsupported (%d published bindings)", unsupported))
	}
	specs := make([]Spec, 0, len(wanted))
	for _, spec := range wanted {
		specs = append(specs, spec)
	}
	if err := w.manager.Sync("docker", specs); err != nil {
		// A busy host port must not disconnect an otherwise healthy event stream.
		w.report(err)
	}
	return nil
}
