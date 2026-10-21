package guest

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func verifyDirectLoop(info *unix.LoopInfo64, backing *unix.Stat_t) error {
	if info.Flags&unix.LO_FLAGS_DIRECT_IO == 0 {
		return errors.New("kernel did not enable loop direct I/O (unsupported backing filesystem or alignment)")
	}
	if info.Flags&unix.LO_FLAGS_AUTOCLEAR == 0 {
		return errors.New("loop device does not have automatic cleanup enabled")
	}
	if info.Flags&unix.LO_FLAGS_READ_ONLY != 0 || info.Offset != 0 || info.Sizelimit != 0 ||
		info.Device != uint64(backing.Dev) || info.Inode != backing.Ino {
		return errors.New("loop device does not match the complete writable machine disk")
	}
	return nil
}

func openDirectLoop(ctx context.Context, path string) (*os.File, error) {
	backing, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer backing.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(backing.Fd()), &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size <= 0 || stat.Size%4096 != 0 {
		return nil, errors.New("machine disk must be a regular file aligned to 4096 bytes")
	}
	control, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open loop control: %w", err)
	}
	defer control.Close()
	for range 16 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		number, err := unix.IoctlRetInt(int(control.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return nil, fmt.Errorf("find unused loop device: %w", err)
		}
		loop, err := os.OpenFile(fmt.Sprintf("/dev/loop%d", number), os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		config := unix.LoopConfig{
			Fd: uint32(backing.Fd()),
			// x/sys names the kernel's block_size field Size.
			Size: 4096,
			Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_DIRECT_IO | unix.LO_FLAGS_AUTOCLEAR},
		}
		copy(config.Info.File_name[:], path)
		if err := unix.IoctlLoopConfigure(int(loop.Fd()), &config); err != nil {
			loop.Close()
			if errors.Is(err, unix.EBUSY) {
				continue
			}
			return nil, fmt.Errorf("configure direct-I/O loop device: %w", err)
		}
		// Linux may silently clear DIRECT_IO for unsupported alignment. Query
		// effective flags rather than trusting successful LOOP_CONFIGURE.
		info, err := unix.IoctlLoopGetStatus64(int(loop.Fd()))
		if err == nil {
			err = verifyDirectLoop(info, &stat)
		}
		if err != nil {
			cleanupErr := unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0)
			loop.Close()
			return nil, errors.Join(err, cleanupErr)
		}
		return loop, nil
	}
	return nil, errors.New("no loop device could be reserved after 16 concurrent-allocation retries")
}

func verifyMountedDirectLoop(root, image string) error {
	var mounted, backing unix.Stat_t
	if err := unix.Stat(root, &mounted); err != nil {
		return err
	}
	if unix.Major(uint64(mounted.Dev)) != 7 {
		return errors.New("machine filesystem is not mounted through a loop device")
	}
	if err := unix.Stat(image, &backing); err != nil {
		return err
	}
	loop, err := os.Open(fmt.Sprintf("/dev/loop%d", unix.Minor(uint64(mounted.Dev))))
	if err != nil {
		return err
	}
	defer loop.Close()
	info, err := unix.IoctlLoopGetStatus64(int(loop.Fd()))
	if err != nil {
		return err
	}
	return verifyDirectLoop(info, &backing)
}
