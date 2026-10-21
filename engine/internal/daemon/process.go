package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"sapphire/engine/internal/config"
	"sapphire/engine/internal/privatefs"
)

type child struct {
	cmd          *exec.Cmd
	done         chan struct{}
	parentWriter *os.File
	waitErr      error // Written before done closes; only read after receiving done.
	logs         []bootLog
}

func driverArguments(c config.Config, initializeDataDisk bool) []string {
	args := []string{
		"--kernel", c.Path("assets/kernel"),
		"--initrd", c.Path("assets/initrd.gz"),
		"--root-disk", c.Path("assets/rootfs.img"),
		"--data-disk", c.Path("data.img"),
		"--memory-mib", fmt.Sprint(c.MemoryMiB),
		"--cpus", fmt.Sprint(c.CPUs),
		"--net-fd", "3",
		"--parent-fd", "5",
		"--vsock", c.Path("vsock.sock"),
		"--console", c.Path("console.log"),
	}
	if initializeDataDisk {
		args = append(args, "--initialize-data-disk")
	}
	return args
}

func startDriver(c config.Config, ethernet, lifetimeLock *os.File, initializeDataDisk bool) (*child, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	driver, err := c.ResolveDriver(executable)
	if err != nil {
		return nil, err
	}
	log, err := privatefs.Open(c.Path("vmm.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	logInfo, err := log.Stat()
	if err != nil {
		return nil, err
	}
	console, err := privatefs.Open(c.Path("console.log"), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	consoleInfo, err := console.Stat()
	console.Close()
	if err != nil {
		return nil, err
	}
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer parentReader.Close()
	cmd := exec.Command(driver, driverArguments(c, initializeDataDisk)...)
	// FD 4 keeps the same flock alive if the daemon crashes, guarding the disks
	// against a second VMM until this child exits. FD 5 EOF stops the native VM.
	cmd.ExtraFiles = []*os.File{ethernet, lifetimeLock, parentReader}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		parentWriter.Close()
		return nil, fmt.Errorf("start VMM: %w", err)
	}
	process := &child{
		cmd: cmd, done: make(chan struct{}), parentWriter: parentWriter,
		logs: []bootLog{
			{path: c.Path("vmm.log"), offset: logInfo.Size(), lines: 12},
			{path: c.Path("console.log"), offset: consoleInfo.Size(), lines: 60},
		},
	}
	go func() {
		process.waitErr = cmd.Wait()
		parentWriter.Close()
		close(process.done)
	}()
	return process, nil
}

// Call only after done closes so Wait's exit status is safe to read.
func (p *child) exitError(phase string) error {
	if p.waitErr != nil {
		return fmt.Errorf("VMM exited%s: %w", phase, p.waitErr)
	}
	return fmt.Errorf("VMM exited%s with status 0", phase)
}

func (p *child) transportError(cause error, phase string) error {
	// Socket hangup can arrive just before Wait observes process exit.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if p.wait(ctx) {
		return errors.Join(p.exitError(phase), cause)
	}
	return cause
}

func (p *child) wait(ctx context.Context) bool {
	select {
	case <-p.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *child) stop(grace time.Duration) error {
	defer p.parentWriter.Close()
	if grace > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if p.wait(ctx) {
			return nil
		}
	}
	// Failed startup has no shutdown to await. EOF also stops a native child
	// that has not installed its signal handlers yet.
	p.parentWriter.Close()
	select {
	case <-p.done:
		return nil
	default:
	}
	// Signal only the os.Process obtained from our own exec.Cmd, never a PID file.
	p.cmd.Process.Signal(syscall.SIGTERM)
	ctx, cancelTerm := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelTerm()
	if p.wait(ctx) {
		return nil
	}
	p.cmd.Process.Kill()
	ctx, cancelKill := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelKill()
	if !p.wait(ctx) {
		return fmt.Errorf("VMM did not exit after SIGKILL")
	}
	return nil
}
