//go:build ignore

// This standalone Linux ARM64 test guest uses only the Go standard library.
package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type sockaddrVM struct {
	Family   uint16
	Reserved uint16
	Port     uint32
	CID      uint32
	Flags    uint8
	Zero     [3]uint8
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "VSOCK_GUEST_FAIL:", err)
		os.Exit(1)
	}
}

func discardTest() {
	file, err := os.OpenFile("/dev/vdb", os.O_RDWR, 0)
	must(err)
	defer file.Close()
	data := make([]byte, 4*1024*1024)
	for i := range data {
		data[i] = 0xa5
	}
	n, err := file.WriteAt(data, 1024*1024)
	must(err)
	if n != len(data) {
		must(fmt.Errorf("short block write: %d", n))
	}
	must(file.Sync())
	extent := [2]uint64{2 * 1024 * 1024, 2 * 1024 * 1024}
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), 0x1277,
		uintptr(unsafe.Pointer(&extent[0])))
	if e != 0 {
		must(e)
	}
	_, err = file.ReadAt(data, 1024*1024)
	must(err)
	for i, b := range data {
		expected := byte(0xa5)
		if i >= 1024*1024 && i < 3*1024*1024 {
			expected = 0
		}
		if b != expected {
			must(fmt.Errorf("discard damaged byte %d: got %d expected %d", i, b, expected))
		}
	}
	fmt.Println("SAPPHIRE_DISCARD_OK bytes=2097152")
}

func main() {
	fd, err := syscall.Socket(40, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	must(err)
	defer syscall.Close(fd)
	addr := sockaddrVM{Family: 40, Port: 1024, CID: ^uint32(0)}
	_, _, e := syscall.RawSyscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if e != 0 {
		must(e)
	}
	must(syscall.Listen(fd, 8))
	fmt.Println("SAPPHIRE_VSOCK_READY")
	conn, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(fd), 0, 0,
		syscall.SOCK_CLOEXEC, 0, 0)
	if e != 0 {
		must(e)
	}
	defer syscall.Close(int(conn))
	var total int
	buf := make([]byte, 32768)
	for {
		n, err := syscall.Read(int(conn), buf)
		if err == syscall.EINTR {
			continue
		}
		must(err)
		if n == 0 {
			break
		}
		total += n
		for done := 0; done < n; {
			written, err := syscall.Write(int(conn), buf[done:n])
			if err == syscall.EINTR {
				continue
			}
			must(err)
			done += written
		}
	}
	fmt.Printf("SAPPHIRE_VSOCK_ECHO_OK bytes=%d\n", total)
	discardTest()
}
