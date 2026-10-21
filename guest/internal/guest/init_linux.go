package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

func CheckBlankDevice(path string) error {
	if path != "/dev/vdb" {
		return errors.New("blank-device check is restricted to /dev/vdb")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return errors.New("/dev/vdb is not a block device")
	}
	size, err := file.Seek(0, io.SeekEnd)
	if err != nil || size < 64<<20 {
		return errors.New("cannot determine a valid data disk size")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	log.Printf("validating all %d bytes of unformatted data disk; no writes yet", size)
	return checkZeroReader(file, size)
}

func ReportInitFailure(err error) {
	log.Printf("machine init: %v", err)
	file := os.NewFile(3, "init-ready")
	if file != nil {
		_ = json.NewEncoder(file).Encode(map[string]string{"error": err.Error()})
		file.Close()
	}
}

func MachineInit(root string) error {
	if os.Getpid() != 1 || os.Geteuid() != 0 || !strings.HasPrefix(root, MachinesPath+"/") || filepath.Base(root) != "rootfs" {
		return errors.New("invalid machine init invocation")
	}
	ready := os.NewFile(3, "init-ready")
	if ready == nil {
		return errors.New("missing init readiness descriptor")
	}
	unix.CloseOnExec(3)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// clone3 placed us in the target cgroup before startup. Re-root the cgroup
	// namespace now, rather than relying on clone namespace/attach ordering.
	if err := unix.Unshare(unix.CLONE_NEWCGROUP); err != nil {
		return fmt.Errorf("unshare machine cgroup namespace: %w", err)
	}
	name := filepath.Base(filepath.Dir(root))
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	if err := unix.Sethostname([]byte(name)); err != nil {
		return err
	}
	initPath := detectInit(root)
	if err := unix.Mount(root, root, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	for _, dir := range []string{"proc", "sys", "dev", "run", ".sapphire-old-root"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}
	mounts := []struct {
		source, target, kind, data string
		flags                      uintptr
	}{
		{"proc", "proc", "proc", "", unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV},
		{"sysfs", "sys", "sysfs", "", unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV | unix.MS_RDONLY},
		{"tmpfs", "dev", "tmpfs", "mode=0755,size=16m", unix.MS_NOSUID},
		{"tmpfs", "run", "tmpfs", "mode=0755,size=64m", unix.MS_NOSUID | unix.MS_NODEV},
	}
	for _, mount := range mounts {
		if err := unix.Mount(mount.source, filepath.Join(root, mount.target), mount.kind, mount.flags, mount.data); err != nil {
			return fmt.Errorf("mount machine %s: %w", mount.target, err)
		}
	}
	// sysfs is read-only, but its existing cgroup directory can be a mountpoint.
	cgroupTarget := filepath.Join(root, "sys/fs/cgroup")
	if err := unix.Mount(machineCgroup(name), cgroupTarget, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("mount machine cgroup: %w", err)
	}
	for _, dir := range []string{"dev/pts", "dev/shm"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("devpts", filepath.Join(root, "dev/pts"), "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", filepath.Join(root, "dev/shm"), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=64m"); err != nil {
		return err
	}
	for _, device := range []struct {
		name         string
		major, minor uint32
	}{{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7}, {"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0}, {"console", 5, 1}} {
		if err := unix.Mknod(filepath.Join(root, "dev", device.name), unix.S_IFCHR|0o666, int(unix.Mkdev(device.major, device.minor))); err != nil {
			return err
		}
	}
	for name, target := range map[string]string{
		"ptmx": "pts/ptmx", "fd": "/proc/self/fd",
		"stdin": "/proc/self/fd/0", "stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2",
	} {
		if err := os.Symlink(target, filepath.Join(root, "dev", name)); err != nil {
			return err
		}
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", ".sapphire-old-root"); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Unmount("/.sapphire-old-root", unix.MNT_DETACH); err != nil {
		return err
	}
	_ = os.Remove("/.sapphire-old-root")
	payload, _ := json.Marshal(map[string]string{"init": initPath})
	if _, err := ready.Write(payload); err != nil {
		return err
	}
	if initPath != "sapphire-init" {
		env := append(guestEnvironment(), "container=sapphire")
		// FD 3 closes only on successful exec; the parent waits for that EOF.
		return unix.Exec(initPath, []string{initPath}, env)
	}
	ready.Close()
	return runMachinePID1()
}

func runMachinePID1() error {
	_ = newSupervisor(readBootID())
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, unix.SIGTERM, unix.SIGINT, unix.SIGPWR)
	defer signal.Stop(signals)
	<-signals
	// Inside this PID namespace, -1 can only target this machine's processes.
	_ = unix.Kill(-1, unix.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	_ = unix.Kill(-1, unix.SIGKILL)
	if root, err := os.Open("/"); err == nil {
		_ = unix.Syncfs(int(root.Fd()))
		root.Close()
	}
	return nil
}

func readBootID() string {
	data, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(data))
}

func RunPID1() {
	_ = os.Setenv("PATH", strings.TrimPrefix(guestEnvironment()[0], "PATH="))
	_ = os.MkdirAll("/var/log/sapphire", 0o700)
	if file, err := os.OpenFile("/var/log/sapphire/guest.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, file))
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Printf("sapphire-guest %s starting, protocol %d", Version, Protocol)
	if err := ValidateWallClock(time.Now()); err != nil {
		log.Printf("clock check failed: %v", err)
	} else {
		log.Printf("guest wall clock: %s; rtc0=%s", time.Now().UTC().Format(time.RFC3339), readText("/sys/class/rtc/rtc0/name"))
	}
	for _, path := range []string{MachinesPath, "/run/sapphire", "/var/lib/docker", "/var/lib/rancher", "/sys/fs/cgroup"} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			holdOnBootFailure(err)
		}
	}
	_ = unix.Sethostname([]byte("sapphire"))
	bootID := readBootID()
	if bootID == "" {
		holdOnBootFailure(errors.New("missing Linux boot ID"))
	}
	s := newSupervisor(bootID)
	cgroupError := prepareCgroups()
	if cgroupError != nil {
		log.Printf("cgroup setup failed: %v; lifecycle actions will be refused", cgroupError)
	}
	a := &agent{
		supervisor: s,
		machines:   &machines{supervisor: s, bootID: bootID},
		services:   &services{supervisor: s},
		bootID:     bootID, cgroupError: cgroupError,
		shutdown: make(chan struct{}, 1),
	}
	a.startNetwork()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.listen(ctx); err != nil {
		holdOnBootFailure(err)
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, unix.SIGTERM, unix.SIGINT, unix.SIGPWR)
	select {
	case <-signals:
	case <-a.shutdown:
	}
	signal.Stop(signals)
	log.Print("guest shutdown requested")
	a.requestMu.Lock()
	a.stopping = true
	a.requestMu.Unlock()
	cancel()
	shutdown, done := context.WithTimeout(context.Background(), 35*time.Second)
	defer done()
	drained := make(chan struct{})
	go func() { a.active.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		log.Print("active guest requests did not drain within shutdown grace")
	}
	infos, _ := a.machines.list()
	var stopping sync.WaitGroup
	for _, machine := range infos {
		if machine.Status == "running" || machine.Status == "starting" {
			stopping.Add(1)
			go func(name string) {
				defer stopping.Done()
				if _, err := a.machines.stop(shutdown, name); err != nil {
					log.Printf("stop machine %s: %v", name, err)
				}
			}(machine.Name)
		}
	}
	_, _ = a.services.stopK3s(shutdown)
	stopping.Wait()
	_, _ = a.services.stopDocker(shutdown)
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		holdOnBootFailure(err)
	}
}

func holdOnBootFailure(err error) {
	log.Printf("FATAL guest initialization: %v", err)
	select {}
}

func (a *agent) startNetwork() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.supervisor.command(ctx, "ip", "link", "set", "lo", "up")
		cancel()
		interfaces, err := net.Interfaces()
		if err != nil {
			log.Printf("network discovery: %v", err)
			return
		}
		for _, iface := range interfaces {
			if iface.Flags&net.FlagLoopback != 0 || !strings.HasPrefix(iface.Name, "eth") {
				continue
			}
			null, err := os.Open(os.DevNull)
			if err != nil {
				return
			}
			defer null.Close()
			attrs, cgroup, err := serviceAttributes("network")
			if err != nil {
				log.Printf("network cgroup: %v", err)
				return
			}
			defer cgroup.Close()
			child, err := a.supervisor.start("udhcpc",
				[]string{"-f", "-i", iface.Name, "-s", "/usr/share/sapphire/udhcpc.script", "-p", "/run/sapphire/udhcpc.pid", "-x", "hostname:sapphire"},
				guestEnvironment(), []*os.File{null, os.Stdout, os.Stderr}, attrs)
			if err != nil {
				log.Printf("DHCP: %v", err)
			} else {
				logChild("DHCP", child)
			}
			return
		}
		log.Print("no eth* interface available; vsock RPC remains available without IP networking")
	}()
}
