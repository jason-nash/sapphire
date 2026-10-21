package privatefs

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var ErrLocked = errors.New("engine is already running or starting")

func Directory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", path)
	}
	return os.Chmod(path, 0700)
}

func Open(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s must be a regular file", path)
	}
	if err == nil {
		err = file.Chmod(0600)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// The lock file stays in place: unlinking a flock file creates two lock domains.
func Lock(home string) (*os.File, error) {
	file, err := Open(filepath.Join(home, "engine.lock"), unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return file, nil
}

func Held(home string) (bool, error) {
	fd, err := unix.Open(filepath.Join(home, "engine.lock"), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// Existing data is never truncated, resized, or replaced.
func DataDisk(path string, size int64) error {
	_, err := EnsureDataDisk(path, size)
	return err
}

// EnsureDataDisk reports creation only after this call exclusively creates and
// truncates the file. Existing files never grant permission to initialize data.
func EnsureDataDisk(path string, size int64) (created bool, err error) {
	if size <= 0 || size%(1<<30) != 0 || size > 2048*(1<<30) {
		return false, errors.New("data disk size must be 1-2048 whole GiB")
	}
	file, err := Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if errors.Is(err, os.ErrExist) {
		existing, openErr := Open(path, unix.O_RDWR)
		if openErr != nil {
			return false, openErr
		}
		defer existing.Close()
		info, statErr := existing.Stat()
		if statErr != nil {
			return false, statErr
		}
		if info.Size() != size {
			return false, fmt.Errorf("existing data disk is %d bytes; configured size is %d; refusing to resize", info.Size(), size)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if err := file.Truncate(size); err != nil {
		os.Remove(path)
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	return true, nil
}

// Call only while holding the lifetime lock.
func RemoveSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket %s", path)
	}
	return os.Remove(path)
}

func Listen(path string) (*net.UnixListener, error) {
	if err := RemoveSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
