package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type child struct {
	identity ProcessIdentity
	process  *os.Process
	pidfd    int
	done     chan struct{}
	exitCode int
}

// All wait4 calls belong to this supervisor, including adopted grandchildren.
// Holding mu through StartProcess prevents a fast exit racing registration.
type supervisor struct {
	mu       sync.Mutex
	children map[int]*child
	signals  chan os.Signal
	bootID   string
}

func newSupervisor(bootID string) *supervisor {
	s := &supervisor{children: make(map[int]*child), signals: make(chan os.Signal, 16), bootID: bootID}
	signal.Notify(s.signals, syscall.SIGCHLD)
	go func() {
		for range s.signals {
			s.reap()
		}
	}()
	s.reap()
	return s
}

func (s *supervisor) reap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		if c, ok := s.children[pid]; ok {
			c.exitCode = status.ExitStatus()
			if status.Signaled() {
				c.exitCode = 128 + int(status.Signal())
			}
			if c.pidfd >= 0 {
				unix.Close(c.pidfd)
				c.pidfd = -1
			}
			_ = c.process.Release()
			delete(s.children, pid)
			close(c.done)
		}
	}
}

func (s *supervisor) start(path string, args, env []string, files []*os.File, attr *syscall.SysProcAttr) (*child, error) {
	if !strings.HasPrefix(path, "/") {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return nil, err
		}
		path = resolved
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	proc, err := os.StartProcess(path, append([]string{path}, args...), &os.ProcAttr{
		Env: env, Files: files, Sys: attr,
	})
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", path, err)
	}
	c := &child{process: proc, done: make(chan struct{}), pidfd: -1}
	c.identity = ProcessIdentity{PID: proc.Pid, BootID: s.bootID}
	c.pidfd, err = unix.PidfdOpen(proc.Pid, 0)
	if err != nil {
		// This kernel is built with pidfd support. Never use an unverified PID.
		_ = proc.Kill()
		s.children[proc.Pid] = c
		return nil, fmt.Errorf("pidfd_open: %w", err)
	}
	start, _, err := readProcessStat(proc.Pid)
	if err != nil {
		_ = unix.PidfdSendSignal(c.pidfd, unix.SIGKILL, nil, 0)
		s.children[proc.Pid] = c
		return nil, fmt.Errorf("read child identity: %w", err)
	}
	c.identity.StartTicks = start
	s.children[proc.Pid] = c
	return c, nil
}

func (s *supervisor) send(c *child, sig unix.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-c.done:
		return nil
	default:
	}
	if c.pidfd < 0 {
		return errors.New("child has no process handle")
	}
	err := unix.PidfdSendSignal(c.pidfd, sig, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func (s *supervisor) stop(ctx context.Context, c *child) error {
	if err := s.send(c, unix.SIGTERM); err != nil {
		return err
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		_ = s.send(c, unix.SIGKILL)
		select {
		case <-c.done:
			return nil
		case <-time.After(3 * time.Second):
			return errors.New("process did not exit after SIGKILL")
		}
	}
}

func guestEnvironment() []string {
	return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C.UTF-8"}
}

func (s *supervisor) capture(ctx context.Context, path string, args []string, attr *syscall.SysProcAttr, extra []*os.File) (CommandResult, error) {
	var result CommandResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	in, err := os.Open(os.DevNull)
	if err != nil {
		return result, err
	}
	defer in.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer errR.Close()
	defer errW.Close()
	c, err := s.start(path, args, guestEnvironment(), append([]*os.File{in, outW, errW}, extra...), attr)
	outW.Close()
	errW.Close()
	if err != nil {
		return result, err
	}
	if attr != nil && attr.UseCgroupFD {
		// Arm cancellation after the child exists, so a disconnect racing spawn
		// cannot kill an empty group and leave the newly spawned subtree alive.
		finished := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			defer close(finished)
			_ = writeValue(fmt.Sprintf("/proc/self/fd/%d/cgroup.kill", attr.CgroupFD), "1")
		})
		defer func() {
			if !stop() {
				<-finished
			}
		}()
	}
	var stdout, stderr limitedBuffer
	var copying sync.WaitGroup
	copying.Add(2)
	go func() { defer copying.Done(); _, _ = io.Copy(&stdout, outR) }()
	go func() { defer copying.Done(); _, _ = io.Copy(&stderr, errR) }()
	select {
	case <-ctx.Done():
		grace, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.stop(grace, c)
		cancel()
	case <-c.done:
	}
	// A command may leave grandchildren holding its pipe descriptors.
	drained := make(chan struct{})
	go func() { copying.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(time.Second):
		outR.Close()
		errR.Close()
		<-drained
	}
	select {
	case <-c.done:
		result.ExitCode = c.exitCode
	default:
		return result, errors.New("command could not be reaped")
	}
	result.Stdout, result.Stderr = string(stdout.data), string(stderr.data)
	result.Truncated = stdout.truncated || stderr.truncated
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func (s *supervisor) command(ctx context.Context, path string, args ...string) error {
	result, err := s.capture(ctx, path, args, nil, nil)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", path, result.ExitCode, strings.TrimSpace(result.Stderr+"\n"+result.Stdout))
	}
	return nil
}

func readProcessStat(pid int) (uint64, byte, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	return ParseProcStat(string(data))
}

func processExists(identity ProcessIdentity, bootID, cgroup string) bool {
	if identity.PID <= 1 || identity.BootID != bootID || identity.StartTicks == 0 {
		return false
	}
	start, state, err := readProcessStat(identity.PID)
	if err != nil || start != identity.StartTicks || state == 'Z' || state == 'X' {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", identity.PID))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path == cgroup || strings.HasPrefix(path, cgroup+"/")
		}
	}
	return false
}

func signalIdentity(identity ProcessIdentity, bootID, cgroup string, sig unix.Signal) error {
	fd, err := unix.PidfdOpen(identity.PID, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if !processExists(identity, bootID, cgroup) {
		return errors.New("process identity no longer matches")
	}
	return unix.PidfdSendSignal(fd, sig, nil, 0)
}

func writeValue(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func prepareCgroups() error {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		if err := unix.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, "nsdelegate"); err != nil {
			return fmt.Errorf("mount cgroup v2: %w", err)
		}
	}
	// The root cgroup is exempt from the no-internal-process constraint.
	if err := writeValue("/sys/fs/cgroup/cgroup.subtree_control", "+cpu +memory +pids"); err != nil {
		return err
	}
	for _, path := range []string{CgroupPath, CgroupPath + "/services", CgroupPath + "/machines", CgroupPath + "/docker"} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	for _, path := range []string{CgroupPath, CgroupPath + "/machines", CgroupPath + "/docker", CgroupPath + "/services"} {
		if err := writeValue(path+"/cgroup.subtree_control", "+cpu +memory +pids"); err != nil {
			return err
		}
	}
	return nil
}

func serviceAttributes(name string) (*syscall.SysProcAttr, *os.File, error) {
	path := CgroupPath + "/services/" + name
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, nil, err
	}
	fd, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(fd.Fd())}, fd, nil
}

func logChild(label string, c *child) {
	go func() {
		<-c.done
		log.Printf("%s exited with status %s", label, strconv.Itoa(c.exitCode))
	}()
}
