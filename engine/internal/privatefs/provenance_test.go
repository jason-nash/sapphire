package privatefs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureDataDiskReportsOnlyExclusiveSuccessfulCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.img")
	const size = int64(1 << 30)
	created, err := EnsureDataDisk(path, size)
	if err != nil || !created {
		t.Fatalf("new sparse disk: created %v, error %v", created, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != size {
		t.Fatalf("creation reported without truncating to configured size: %v %v", info, err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	marker := []byte("existing bytes remain unchanged")
	if _, err := file.WriteAt(marker, 4096); err != nil {
		t.Fatal(err)
	}
	created, err = EnsureDataDisk(path, size)
	if err != nil || created {
		t.Fatalf("existing disk: created %v, error %v", created, err)
	}
	persisted := make([]byte, len(marker))
	if _, err := file.ReadAt(persisted, 4096); err != nil || !bytes.Equal(persisted, marker) {
		t.Fatalf("existing disk changed: %q %v", persisted, err)
	}
	for _, invalidSize := range []int64{0, 2 * size} {
		created, err := EnsureDataDisk(path, invalidSize)
		if err == nil || created {
			t.Fatalf("failed disk preparation claimed creation: size %d, created %v, error %v", invalidSize, created, err)
		}
	}
	link := filepath.Join(t.TempDir(), "data.img")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if created, err := EnsureDataDisk(link, size); err == nil || created {
		t.Fatalf("symlink granted creation permission: %v %v", created, err)
	}
}

func TestEnsureDataDiskHasOnlyOneExclusiveCreator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.img")
	const contenders = 8
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, contenders)
	for range contenders {
		go func() {
			created, err := EnsureDataDisk(path, 1<<30)
			results <- result{created: created, err: err}
		}()
	}
	creators := 0
	for range contenders {
		result := <-results
		if result.created {
			if result.err != nil {
				t.Fatalf("failed creation granted initialization permission: %v", result.err)
			}
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("expected one exclusive creator, got %d", creators)
	}
	if created, err := EnsureDataDisk(path, 1<<30); err != nil || created {
		t.Fatalf("completed existing disk gained creation permission: %v %v", created, err)
	}
}
