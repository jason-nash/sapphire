package guest

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type machines struct {
	supervisor *supervisor
	bootID     string
	locksMu    sync.Mutex
	locks      map[string]*machineLock
}

type machineLock struct {
	token chan struct{}
	refs  int
}

func (m *machines) lock(ctx context.Context, name string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.locksMu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*machineLock)
	}
	lock := m.locks[name]
	if lock == nil {
		lock = &machineLock{token: make(chan struct{}, 1)}
		m.locks[name] = lock
	}
	// A reference covers each holder and waiter. Deletion while any waiter
	// remains would allow a new channel to bypass the existing lock.
	lock.refs++
	m.locksMu.Unlock()
	releaseReference := func() {
		m.locksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(m.locks, name)
		}
		m.locksMu.Unlock()
	}
	select {
	case lock.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-lock.token
			releaseReference()
			return nil, err
		}
		return func() {
			<-lock.token
			releaseReference()
		}, nil
	case <-ctx.Done():
		releaseReference()
		return nil, ctx.Err()
	}
}

func machinePath(name string) string    { return filepath.Join(MachinesPath, name) }
func machineCgroup(name string) string  { return CgroupPath + "/machines/" + name }
func relativeCgroup(name string) string { return "/sapphire/machines/" + name }

func readState(name string) (MachineState, error) {
	var state MachineState
	if err := ValidateName(name); err != nil {
		return state, err
	}
	data, err := os.ReadFile(filepath.Join(machinePath(name), "state.json"))
	if err != nil {
		return state, fmt.Errorf("read machine %s: %w", name, err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("invalid machine state %s: %w", name, err)
	}
	if state.Configuration.Name != name {
		return state, errors.New("machine configuration name does not match its directory")
	}
	return state, state.Configuration.Validate()
}

func atomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (m *machines) info(state MachineState) MachineInfo {
	config := state.Configuration
	info := MachineInfo{
		Name: config.Name, Status: "stopped", Arch: "aarch64", CPUs: config.CPUs,
		Memory: uint64(config.MemoryGiB) << 30, Disk: uint64(config.DiskGiB) << 30,
		Dir: machinePath(config.Name), Configuration: config, InitSystem: state.InitSystem,
		Source: state.Source, CreatedAt: state.CreatedAt,
	}
	if processExists(state.Process, m.bootID, relativeCgroup(config.Name)) {
		info.Status, info.Process = "running", &state.Process
		if state.Booting {
			info.Status = "starting"
		}
	}
	return info
}

func (m *machines) list() ([]MachineInfo, error) {
	entries, err := os.ReadDir(MachinesPath)
	if err != nil {
		return nil, err
	}
	infos := make([]MachineInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		state, err := readState(entry.Name())
		if err != nil {
			infos = append(infos, MachineInfo{Name: entry.Name(), Status: "error", Warning: err.Error()})
			continue
		}
		infos = append(infos, m.info(state))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

func mountDisk(ctx context.Context, s *supervisor, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root := filepath.Join(directory, "rootfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	image := filepath.Join(directory, "disk.img")
	if mounted(root) {
		return verifyMountedDirectLoop(root, image)
	}
	loop, err := openDirectLoop(ctx, image)
	if err != nil {
		return fmt.Errorf("machine disk requires direct I/O; buffered loop fallback is disabled: %w", err)
	}
	// Hold this descriptor until mount takes its own reference. Autoclear then
	// releases the loop device on unmount, or here if mount fails/cancels.
	defer loop.Close()
	if err := s.command(ctx, "mount", "-t", "ext4", "-o", "noatime", loop.Name(), root); err != nil {
		if mounted(root) {
			return errors.Join(err, unix.Unmount(root, 0))
		}
		return err
	}
	if err := verifyMountedDirectLoop(root, image); err != nil {
		return errors.Join(err, unix.Unmount(root, 0))
	}
	return nil
}

func mounted(path string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == path {
			return true
		}
	}
	return false
}

func unmountDisk(directory string) error {
	root := filepath.Join(directory, "rootfs")
	if !mounted(root) {
		return nil
	}
	// A normal unmount is deliberate: never remove a still-mounted filesystem.
	return unix.Unmount(root, 0)
}

func (m *machines) create(ctx context.Context, config MachineConfiguration) (MachineInfo, error) {
	if err := config.Validate(); err != nil {
		return MachineInfo{}, err
	}
	unlock, err := m.lock(ctx, config.Name)
	if err != nil {
		return MachineInfo{}, err
	}
	defer unlock()
	if _, err := os.Lstat(machinePath(config.Name)); !errors.Is(err, os.ErrNotExist) {
		return MachineInfo{}, errors.New("machine already exists, or its directory cannot be accessed")
	}
	stage, err := os.MkdirTemp(MachinesPath, ".create-")
	if err != nil {
		return MachineInfo{}, err
	}
	defer func() {
		if err := unmountDisk(stage); err == nil {
			os.RemoveAll(stage)
		}
	}()
	disk, err := os.OpenFile(filepath.Join(stage, "disk.img"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return MachineInfo{}, err
	}
	err = disk.Truncate(int64(config.DiskGiB) << 30)
	closeErr := disk.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return MachineInfo{}, err
	}
	if err := m.supervisor.command(ctx, "mkfs.ext4", "-q", "-F", "-E", "nodiscard,lazy_itable_init=1,lazy_journal_init=1", filepath.Join(stage, "disk.img")); err != nil {
		return MachineInfo{}, err
	}
	if err := mountDisk(ctx, m.supervisor, stage); err != nil {
		return MachineInfo{}, err
	}
	archive, source, err := m.rootfsArchive(ctx, config.Distribution, stage)
	if err != nil {
		return MachineInfo{}, err
	}
	defer archive.Close()
	var reader io.Reader = archive
	if strings.HasSuffix(archive.Name(), ".gz") {
		gz, err := gzip.NewReader(archive)
		if err != nil {
			return MachineInfo{}, err
		}
		defer gz.Close()
		reader = gz
	}
	reader = contextReader{ctx: ctx, reader: reader}
	root := filepath.Join(stage, "rootfs")
	if err := extractRootfs(root, reader, int64(config.DiskGiB)<<30, true, rootfsXattrs); err != nil {
		return MachineInfo{}, err
	}
	if err := personalizeRootfs(root, config.Name); err != nil {
		return MachineInfo{}, err
	}
	state := MachineState{Configuration: config, CreatedAt: time.Now().UTC(), Source: source}
	state.InitSystem = detectInit(root)
	if err := atomicJSON(filepath.Join(stage, "state.json"), state); err != nil {
		return MachineInfo{}, err
	}
	// The export is only a bootstrap artifact, not persistent machine storage.
	if archive.Name() == filepath.Join(stage, "export.tar") {
		archive.Close()
		os.Remove(archive.Name())
	}
	if err := unmountDisk(stage); err != nil {
		return MachineInfo{}, err
	}
	if err := os.Rename(stage, machinePath(config.Name)); err != nil {
		return MachineInfo{}, err
	}
	return m.info(state), nil
}

func (m *machines) rootfsArchive(ctx context.Context, distribution, stage string) (*os.File, string, error) {
	asset := "/usr/share/sapphire/rootfs/" + distribution + ".tar.gz"
	if _, err := os.Stat(asset); err == nil {
		checksum, err := os.ReadFile(asset + ".sha256")
		if err != nil {
			return nil, "", err
		}
		file, err := os.Open(asset)
		if err != nil {
			return nil, "", err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		expected := strings.Fields(string(checksum))
		if err != nil || len(expected) == 0 || hex.EncodeToString(hash.Sum(nil)) != expected[0] {
			file.Close()
			return nil, "", errors.New("bundled rootfs checksum mismatch")
		}
		_, err = file.Seek(0, io.SeekStart)
		if err != nil {
			file.Close()
			return nil, "", err
		}
		return file, "bundled:" + distribution + "@sha256:" + expected[0], nil
	}
	if !dockerReady(ctx) {
		return nil, "", errors.New("this distribution has no bundled rootfs; call docker.start first to extract its pinned official OCI image")
	}
	image := distributionImages[distribution]
	if err := m.supervisor.command(ctx, "docker", "--host", "unix:///var/run/docker.sock", "pull", "--platform", "linux/arm64", image); err != nil {
		return nil, "", err
	}
	containerName := "sapphire-rootfs-" + filepath.Base(stage)[1:]
	result, err := m.supervisor.capture(ctx, "docker", []string{"--host", "unix:///var/run/docker.sock", "create", "--name", containerName, "--platform", "linux/arm64", image}, nil, nil)
	if err != nil || result.ExitCode != 0 {
		return nil, "", fmt.Errorf("create rootfs extraction container: %v %s", err, result.Stderr)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = m.supervisor.command(cleanup, "docker", "--host", "unix:///var/run/docker.sock", "rm", containerName)
	}()
	export := filepath.Join(stage, "export.tar")
	if err := m.supervisor.command(ctx, "docker", "--host", "unix:///var/run/docker.sock", "export", "--output", export, containerName); err != nil {
		return nil, "", err
	}
	file, err := os.Open(export)
	return file, image, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func rootfsXattrs(file *os.File, header *tar.Header) error {
	for key, value := range header.PAXRecords {
		name, ok := strings.CutPrefix(key, "SCHILY.xattr.")
		if !ok || name == "security.selinux" {
			continue
		}
		if err := unix.Fsetxattr(int(file.Fd()), name, []byte(value), 0); err != nil {
			return fmt.Errorf("preserve xattr %s: %w", name, err)
		}
	}
	return nil
}

func personalizeRootfs(directory, name string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, dir := range []string{"etc", "dev", "proc", "sys", "run", "tmp", "root", "var", "sys/fs/cgroup"} {
		path, err := resolveInside(root, dir)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	values := map[string]string{
		"etc/hostname":   name + "\n",
		"etc/hosts":      "127.0.0.1 localhost\n::1 localhost\n127.0.1.1 " + name + "\n",
		"etc/machine-id": hex.EncodeToString(id[:]) + "\n",
	}
	for path, value := range values {
		// Replace symlinks rather than following a distro's absolute link.
		if info, err := root.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if err := root.Remove(path); err != nil {
				return err
			}
		}
		if err := root.WriteFile(path, []byte(value), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func detectInit(directory string) string {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "sapphire-init"
	}
	defer root.Close()
	for _, candidate := range []string{"sbin/openrc-init", "sbin/init"} {
		resolved, err := resolveInside(root, candidate)
		if err != nil {
			continue
		}
		info, err := root.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			if filepath.Base(resolved) == "busybox" && !usableBusyboxInit(root) {
				continue
			}
			if strings.HasSuffix(resolved, "/systemd") {
				return "/" + resolved
			}
			return "/" + candidate
		}
	}
	return "sapphire-init"
}

func usableBusyboxInit(root *os.Root) bool {
	path, err := resolveInside(root, "etc/inittab")
	if err != nil {
		return false
	}
	file, err := root.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return false
	}
	entries := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, ":", 4)
		if len(fields) != 4 {
			return false
		}
		terminal, action := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[2])
		// Machine /dev is private and provides no virtual or serial getty
		// terminals. BusyBox would otherwise respawn failed gettys forever.
		if (action == "respawn" || action == "askfirst") && terminal != "" && terminal != "console" {
			return false
		}
		command := strings.Fields(fields[3])
		if len(command) == 0 {
			return false
		}
		executable := strings.TrimPrefix(command[0], "-")
		if !strings.HasPrefix(executable, "/") {
			return false
		}
		resolved, err := resolveInside(root, executable)
		if err != nil {
			return false
		}
		info, err := root.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return false
		}
		entries++
	}
	return entries > 0
}

func (m *machines) start(ctx context.Context, name string) (MachineInfo, error) {
	if err := ValidateName(name); err != nil {
		return MachineInfo{}, err
	}
	unlock, err := m.lock(ctx, name)
	if err != nil {
		return MachineInfo{}, err
	}
	defer unlock()
	state, err := readState(name)
	if err != nil {
		return MachineInfo{}, err
	}
	if m.info(state).Status == "running" {
		return m.info(state), nil
	}
	cgroup := machineCgroup(name)
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		return MachineInfo{}, err
	}
	if populated(cgroup) {
		return MachineInfo{}, errors.New("machine cgroup still has processes but its init identity is stale; stop it before restarting")
	}
	for key, value := range map[string]string{
		"memory.max":      strconv.FormatInt(int64(state.Configuration.MemoryGiB)<<30, 10),
		"memory.swap.max": "0", "memory.oom.group": "1", "pids.max": "4096",
		"cpu.max": strconv.Itoa(state.Configuration.CPUs*100000) + " 100000",
	} {
		if err := writeValue(cgroup+"/"+key, value); err != nil {
			return MachineInfo{}, err
		}
	}
	if err := mountDisk(ctx, m.supervisor, machinePath(name)); err != nil {
		return MachineInfo{}, err
	}
	root := machinePath(name) + "/rootfs"
	if err := refreshDNS(root); err != nil {
		_ = unmountDisk(machinePath(name))
		return MachineInfo{}, err
	}
	cg, err := os.Open(cgroup)
	if err != nil {
		return MachineInfo{}, err
	}
	defer cg.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return MachineInfo{}, err
	}
	defer readyR.Close()
	defer readyW.Close()
	logFile, err := os.OpenFile(machinePath(name)+"/boot.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return MachineInfo{}, err
	}
	defer logFile.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return MachineInfo{}, err
	}
	defer null.Close()
	attrs := &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWCGROUP,
		UseCgroupFD: true, CgroupFD: int(cg.Fd()),
	}
	c, err := m.supervisor.start("/sbin/sapphire-guest", []string{"__machine-init", root}, guestEnvironment(),
		[]*os.File{null, logFile, logFile, readyW}, attrs)
	readyW.Close()
	if err != nil {
		_ = unmountDisk(machinePath(name))
		return MachineInfo{}, err
	}
	logChild("machine "+name, c)
	state.Process = c.identity
	state.Booting = true
	if err := atomicJSON(machinePath(name)+"/state.json", state); err != nil {
		_ = m.supervisor.send(c, unix.SIGKILL)
		return MachineInfo{}, err
	}
	type initReport struct {
		Init  string `json:"init"`
		Error string `json:"error"`
	}
	ready := make(chan initReport, 1)
	go func() {
		var response initReport
		data, err := io.ReadAll(io.LimitReader(readyR, 8192))
		if err == nil {
			err = json.Unmarshal(data, &response)
			if err == nil && response.Init == "" {
				err = errors.New("machine init did not report readiness")
			}
		}
		if err != nil {
			response.Error = err.Error()
		}
		ready <- response
	}()
	select {
	case response := <-ready:
		if response.Error != "" {
			err = errors.New(response.Error)
		} else {
			state.InitSystem = response.Init
		}
	case <-ctx.Done():
		err = ctx.Err()
	case <-time.After(20 * time.Second):
		err = errors.New("machine init timed out; inspect boot.log")
	}
	if err == nil && !processExists(c.identity, m.bootID, relativeCgroup(name)) {
		err = errors.New("machine init exited during boot; inspect boot.log")
	}
	if err != nil {
		_ = m.supervisor.send(c, unix.SIGKILL)
		return MachineInfo{}, err
	}
	state.Booting = false
	if err := atomicJSON(machinePath(name)+"/state.json", state); err != nil {
		_ = m.supervisor.send(c, unix.SIGKILL)
		return MachineInfo{}, err
	}
	return m.info(state), nil
}

func refreshDNS(directory string) error {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat("etc/resolv.conf"); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := root.Remove("etc/resolv.conf"); err != nil {
			return err
		}
	}
	return root.WriteFile("etc/resolv.conf", data, 0o644)
}

func populated(cgroup string) bool {
	data, err := os.ReadFile(cgroup + "/cgroup.events")
	return err == nil && strings.Contains(string(data), "populated 1")
}

func (m *machines) stop(ctx context.Context, name string) (MachineInfo, error) {
	if err := ValidateName(name); err != nil {
		return MachineInfo{}, err
	}
	unlock, err := m.lock(ctx, name)
	if err != nil {
		return MachineInfo{}, err
	}
	defer unlock()
	state, err := readState(name)
	if err != nil {
		return MachineInfo{}, err
	}
	if processExists(state.Process, m.bootID, relativeCgroup(name)) {
		// systemd understands SIGRTMIN+3 as halt; OpenRC/our init use SIGTERM.
		sig := unix.SIGTERM
		if strings.HasSuffix(state.InitSystem, "/systemd") {
			sig = unix.Signal(37) // Linux SIGRTMIN (34) + 3.
		}
		if err := signalIdentity(state.Process, m.bootID, relativeCgroup(name), sig); err != nil && !errors.Is(err, unix.ESRCH) {
			return MachineInfo{}, err
		}
	}
	cgroup := machineCgroup(name)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for populated(cgroup) {
		select {
		case <-ctx.Done():
			return MachineInfo{}, ctx.Err()
		case <-deadline.C:
			// The path is derived from a validated name, not a stale PID.
			if err := writeValue(cgroup+"/cgroup.kill", "1"); err != nil {
				return MachineInfo{}, err
			}
			wait := time.NewTimer(3 * time.Second)
			defer wait.Stop()
			for populated(cgroup) {
				select {
				case <-wait.C:
					return MachineInfo{}, errors.New("machine cgroup did not drain")
				case <-ctx.Done():
					return MachineInfo{}, ctx.Err()
				case <-tick.C:
				}
			}
		case <-tick.C:
		}
	}
	if err := unmountDisk(machinePath(name)); err != nil {
		return MachineInfo{}, fmt.Errorf("unmount stopped machine: %w", err)
	}
	state.Process = ProcessIdentity{}
	state.Booting = false
	if err := atomicJSON(machinePath(name)+"/state.json", state); err != nil {
		return MachineInfo{}, err
	}
	return m.info(state), nil
}

func (m *machines) delete(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	unlock, err := m.lock(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := readState(name)
	if err != nil {
		return err
	}
	if m.info(state).Status == "running" || populated(machineCgroup(name)) {
		return errors.New("stop the machine before deleting it")
	}
	if err := unmountDisk(machinePath(name)); err != nil {
		return err
	}
	if err := os.RemoveAll(machinePath(name)); err != nil {
		return err
	}
	_ = os.Remove(machineCgroup(name))
	return nil
}

func (m *machines) clone(ctx context.Context, source, destination string) (MachineInfo, error) {
	if err := ValidateName(source); err != nil {
		return MachineInfo{}, err
	}
	if err := ValidateName(destination); err != nil {
		return MachineInfo{}, err
	}
	if source == destination {
		return MachineInfo{}, errors.New("clone source and destination must differ")
	}
	order := []string{source, destination}
	sort.Strings(order)
	unlock1, err := m.lock(ctx, order[0])
	if err != nil {
		return MachineInfo{}, err
	}
	defer unlock1()
	unlock2, err := m.lock(ctx, order[1])
	if err != nil {
		return MachineInfo{}, err
	}
	defer unlock2()
	state, err := readState(source)
	if err != nil {
		return MachineInfo{}, err
	}
	if m.info(state).Status == "running" || populated(machineCgroup(source)) {
		return MachineInfo{}, errors.New("stop the source before cloning its filesystem")
	}
	if _, err := os.Lstat(machinePath(destination)); !errors.Is(err, os.ErrNotExist) {
		return MachineInfo{}, errors.New("destination already exists or cannot be accessed")
	}
	if err := unmountDisk(machinePath(source)); err != nil {
		return MachineInfo{}, err
	}
	stage, err := os.MkdirTemp(MachinesPath, ".clone-")
	if err != nil {
		return MachineInfo{}, err
	}
	defer func() {
		if unmountDisk(stage) == nil {
			os.RemoveAll(stage)
		}
	}()
	if err := m.supervisor.command(ctx, "cp", "--sparse=always", "--reflink=auto", machinePath(source)+"/disk.img", stage+"/disk.img"); err != nil {
		return MachineInfo{}, err
	}
	if err := m.supervisor.command(ctx, "tune2fs", "-U", "random", stage+"/disk.img"); err != nil {
		return MachineInfo{}, err
	}
	if err := mountDisk(ctx, m.supervisor, stage); err != nil {
		return MachineInfo{}, err
	}
	if err := personalizeRootfs(stage+"/rootfs", destination); err != nil {
		return MachineInfo{}, err
	}
	state.Configuration.Name = destination
	state.Process = ProcessIdentity{}
	state.Booting = false
	state.CreatedAt = time.Now().UTC()
	if err := atomicJSON(stage+"/state.json", state); err != nil {
		return MachineInfo{}, err
	}
	if err := unmountDisk(stage); err != nil {
		return MachineInfo{}, err
	}
	if err := os.Rename(stage, machinePath(destination)); err != nil {
		return MachineInfo{}, err
	}
	return m.info(state), nil
}

func (m *machines) exec(ctx context.Context, name string, arguments []string) (CommandResult, error) {
	if err := ValidateName(name); err != nil {
		return CommandResult{}, err
	}
	if len(arguments) == 0 {
		return CommandResult{}, errors.New("machine.exec requires a command (there is no RPC PTY)")
	}
	unlock, err := m.lock(ctx, name)
	if err != nil {
		return CommandResult{}, err
	}
	state, err := readState(name)
	if err != nil {
		unlock()
		return CommandResult{}, err
	}
	if !processExists(state.Process, m.bootID, relativeCgroup(name)) {
		unlock()
		return CommandResult{}, errors.New("machine is not running")
	}
	// Pin namespace/root descriptors, then verify starttime again before exec.
	var files []*os.File
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	var args []string
	for _, ns := range []string{"mnt", "pid", "uts", "ipc", "cgroup"} {
		file, openErr := os.Open(fmt.Sprintf("/proc/%d/ns/%s", state.Process.PID, ns))
		if openErr != nil {
			unlock()
			return CommandResult{}, openErr
		}
		flag := map[string]string{"mnt": "mount", "pid": "pid", "uts": "uts", "ipc": "ipc", "cgroup": "cgroup"}[ns]
		args = append(args, "--"+flag+"=/proc/self/fd/"+strconv.Itoa(3+len(files)))
		files = append(files, file)
	}
	root, err := os.Open(fmt.Sprintf("/proc/%d/root", state.Process.PID))
	if err != nil {
		unlock()
		return CommandResult{}, err
	}
	args = append(args, "--root=/proc/self/fd/"+strconv.Itoa(3+len(files)), "--wd=/", "--")
	files = append(files, root)
	if !processExists(state.Process, m.bootID, relativeCgroup(name)) {
		unlock()
		return CommandResult{}, errors.New("machine exited while opening its namespaces")
	}
	execGroup, err := os.MkdirTemp(machineCgroup(name), "exec-")
	if err != nil {
		unlock()
		return CommandResult{}, err
	}
	cg, err := os.Open(execGroup)
	unlock()
	if err != nil {
		return CommandResult{}, err
	}
	defer cg.Close()
	defer os.Remove(execGroup)
	args = append(args, arguments...)
	return m.supervisor.capture(ctx, "nsenter", args, &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd())}, files)
}
