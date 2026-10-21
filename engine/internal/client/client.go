package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"sapphire/engine/internal/config"
	"sapphire/engine/internal/daemon"
	"sapphire/engine/internal/privatefs"
	"sapphire/engine/internal/protocol"
)

type Client struct {
	config    config.Config
	http      *http.Client
	transport *http.Transport
}

func New(c config.Config) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.Path("control.sock"))
		},
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	return &Client{config: c, http: &http.Client{Transport: transport}, transport: transport}
}

func (c *Client) Close() { c.transport.CloseIdleConnections() }

func (c *Client) request(ctx context.Context, method, path string, value, output any) error {
	var body bytes.Buffer
	if value != nil {
		if err := json.NewEncoder(&body).Encode(value); err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://sapphire"+path, &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("engine returned HTTP %d", response.StatusCode)
	}
	return protocol.Decode(response.Body, output)
}

func (c *Client) Status(ctx context.Context) (daemon.HostStatus, error) {
	var status daemon.HostStatus
	err := c.request(ctx, http.MethodGet, "/status", nil, &status)
	if unavailable(err) {
		status = daemon.Stopped(c.config)
		held, lockErr := privatefs.Held(c.config.Home)
		if lockErr != nil {
			return status, lockErr
		}
		if held {
			status.State = "unavailable"
			status.Warnings = []string{"lifetime lock is owned but control socket is unavailable; another daemon or VMM may still be starting or running"}
		}
		return status, nil
	}
	if err == nil && (status.Backend != "hypervisor" || status.State == "" ||
		(status.Running && (status.PID <= 0 || status.CPUs < 1 || status.MemoryMiB < 256))) {
		err = errors.New("invalid host status returned by engine")
	}
	return status, err
}

func (c *Client) Call(ctx context.Context, request protocol.Request) protocol.Response {
	var response protocol.Response
	if err := c.request(ctx, http.MethodPost, "/call", request, &response); err != nil {
		return protocol.Failure(err)
	}
	return response
}

func (c *Client) Stop(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.StopTimeout+12*time.Second)
	defer cancel()
	var response protocol.Response
	if err := c.request(ctx, http.MethodPost, "/stop", nil, &response); err != nil {
		if unavailable(err) {
			if held, lockErr := privatefs.Held(c.config.Home); lockErr != nil {
				return lockErr
			} else if held {
				return errors.New("engine owns its lifetime lock but control is unavailable; refusing to signal an unverified process")
			}
			return nil
		}
		return err
	}
	if err := response.Err(); err != nil {
		return err
	}
	// Wait on the lifetime flock, not on stale PIDs or repeated status requests.
	return waitUnlocked(ctx, c.config.Home)
}

func unavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
