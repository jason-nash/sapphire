package privatefs

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPermissionsLockAndSocketSafety(t *testing.T) {
	temp, err := os.MkdirTemp("/tmp", "sf-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(temp)
	dir := filepath.Join(temp, "private")
	if err := Directory(dir); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal(info.Mode())
	}
	lock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Lock(dir); !errors.Is(err, ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("duplicate lifetime lock: %v", err)
	}
	before, _ := lock.Stat()
	lock.Close()
	lock, err = Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := lock.Stat()
	if !os.SameFile(before, after) {
		t.Fatal("lock inode changed")
	}
	defer lock.Close()
	path := filepath.Join(dir, "control.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	listener.Close()
	os.WriteFile(path, []byte("preserve"), 0600)
	if err := RemoveSocket(path); err == nil {
		t.Fatal("removed non-socket")
	}
}

func TestLifetimeLockSurvivesOwnerCloseInInheritedChild(t *testing.T) {
	dir := t.TempDir()
	lock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInheritedLockHelperProcess$")
	child.Env = append(os.Environ(), "SAPPHIRE_LOCK_FIXTURE=1")
	child.ExtraFiles = []*os.File{lock}
	child.Stdin = reader
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	reader.Close()
	lock.Close()
	held, err := Held(dir)
	writer.Close()
	waitErr := child.Wait()
	if err != nil || !held || waitErr != nil {
		t.Fatalf("inherited guard: held %v err %v child %v", held, err, waitErr)
	}
	if held, err := Held(dir); held || err != nil {
		t.Fatalf("guard survived child exit: %v %v", held, err)
	}
}

func TestInheritedLockHelperProcess(t *testing.T) {
	if os.Getenv("SAPPHIRE_LOCK_FIXTURE") != "1" {
		return
	}
	file := os.NewFile(3, "inherited-lock")
	if _, err := file.Stat(); err != nil {
		os.Exit(2)
	}
	io.Copy(io.Discard, os.Stdin)
	file.Close()
	os.Exit(0)
}

func TestSparseDiskNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.img")
	const size = int64(1 << 30)
	if err := DataDisk(path, size); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteAt([]byte("data survives"), 4096); err != nil {
		t.Fatal(err)
	}
	if err := DataDisk(path, size); err != nil {
		t.Fatal(err)
	}
	if err := DataDisk(path, 2*size); err == nil {
		t.Fatal("resized existing data")
	}
	if err := DataDisk(path, 0); err == nil {
		t.Fatal("accepted zero size")
	}
	content := make([]byte, len("data survives"))
	file.ReadAt(content, 4096)
	info, _ := file.Stat()
	if string(content) != "data survives" || info.Size() != size {
		t.Fatalf("disk modified: %q size %d", content, info.Size())
	}
}

func TestSymlinksAreNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("preserve"), 0600)
	link := filepath.Join(dir, "data.img")
	os.Symlink(target, link)
	if err := DataDisk(link, 1<<30); err == nil {
		t.Fatal("followed disk symlink")
	}
	directoryLink := filepath.Join(dir, "directory")
	os.Symlink(dir, directoryLink)
	if err := Directory(directoryLink); err == nil {
		t.Fatal("followed directory symlink")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "preserve" {
		t.Fatal("modified symlink target")
	}
}
