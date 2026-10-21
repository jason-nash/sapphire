package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"sapphire/engine/internal/config"
	"sapphire/engine/internal/daemon"
	"sapphire/engine/internal/privatefs"
	"sapphire/engine/internal/protocol"
)

func waitUnlocked(ctx context.Context, home string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// flock has no cancellable wait. Finite, coarse retries are confined to stop.
	delay := 100 * time.Millisecond
	for {
		lock, err := privatefs.Lock(home)
		if err == nil {
			lock.Close()
			return nil
		}
		if !errors.Is(err, privatefs.ErrLocked) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for engine shutdown: %w", ctx.Err())
		case <-timer.C:
			delay = min(delay*2, time.Second)
		}
	}
}

func (c *Client) Start(ctx context.Context, executable string) (daemon.HostStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.StartupTimeout+5*time.Second)
	defer cancel()
	status, err := c.Status(ctx)
	if err != nil || status.Running {
		return status, err
	}
	if status.State == "starting" {
		return c.waitRunning(ctx)
	}
	if status.State == "unavailable" {
		return status, errors.New("engine or VMM still owns the lifetime lock; control is unavailable; refusing to launch another VMM")
	}
	if status.State == "stopping" {
		return status, errors.New("engine is stopping; wait for stop to finish before restarting")
	}
	if err := privatefs.Directory(c.config.Home); err != nil {
		return status, err
	}
	log, err := privatefs.Open(c.config.Path("engine.log"), unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND)
	if err != nil {
		return status, err
	}
	defer log.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		return status, err
	}
	defer reader.Close()
	defer writer.Close()
	cmd := exec.Command(executable, serveArguments(c.config)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{writer}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return status, err
	}
	writer.Close()
	// Reap only the daemon we just spawned. It owns all VMM lifecycle decisions.
	go cmd.Wait()
	result := make(chan error, 1)
	go func() {
		var response protocol.Response
		if err := protocol.Decode(reader, &response); err != nil {
			result <- err
		} else {
			result <- response.Err()
		}
	}()
	select {
	case <-ctx.Done():
		return status, fmt.Errorf("engine startup: %w (see %s)", ctx.Err(), c.config.Path("engine.log"))
	case err := <-result:
		if err != nil {
			// Concurrent start lost the lock: join the winner instead of spawning.
			if err.Error() == privatefs.ErrLocked.Error() {
				return c.waitRunning(ctx)
			}
			return status, fmt.Errorf("engine startup: %w (see %s)", err, c.config.Path("engine.log"))
		}
	}
	status, err = c.Status(ctx)
	if err == nil && !status.Running {
		err = fmt.Errorf("engine exited after startup readiness (see %s)", c.config.Path("engine.log"))
	}
	return status, err
}

func serveArguments(c config.Config) []string {
	args := []string{
		"serve", "--home", c.Home, "--cpus", fmt.Sprint(c.CPUs),
		"--memory-mib", fmt.Sprint(c.MemoryMiB), "--data-gib", fmt.Sprint(c.DataGiB),
		"--subnet", c.Subnet, "--mac", c.MAC,
		"--startup-timeout", c.StartupTimeout.String(), "--stop-timeout", c.StopTimeout.String(),
		"--ready-fd", "3",
	}
	if c.Driver != "" {
		args = append(args, "--vmm", c.Driver)
	}
	return args
}

func (c *Client) waitRunning(ctx context.Context) (daemon.HostStatus, error) {
	delay := 250 * time.Millisecond
	seenStarting := false
	for {
		status, err := c.Status(ctx)
		if err != nil || status.Running {
			return status, err
		}
		switch status.State {
		case "starting":
			seenStarting = true
		case "stopped", "stopping":
			return status, fmt.Errorf("existing engine %s before startup readiness (see %s)", status.State, c.config.Path("engine.log"))
		case "unavailable":
			if seenStarting {
				return status, fmt.Errorf("existing engine disappeared before startup readiness (see %s)", c.config.Path("engine.log"))
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, fmt.Errorf("waiting for existing engine: %w", ctx.Err())
		case <-timer.C:
			delay = min(2*delay, time.Second)
		}
	}
}
