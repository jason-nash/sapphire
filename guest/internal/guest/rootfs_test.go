package guest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

type tarEntry struct {
	header tar.Header
	data   string
}

func archive(t *testing.T, entries []tarEntry) *bytes.Reader {
	t.Helper()
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := entry.header
		header.Size = int64(len(entry.data))
		if header.Mode == 0 {
			header.Mode = 0o755
		}
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buffer.Bytes())
}

func TestRootfsExtractionMergedUsr(t *testing.T) {
	directory := t.TempDir()
	reader := archive(t, []tarEntry{
		{tar.Header{Name: "usr/bin", Typeflag: tar.TypeDir}, ""},
		{tar.Header{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "/usr/bin"}, ""},
		{tar.Header{Name: "bin/sh", Typeflag: tar.TypeReg}, "shell"},
		{tar.Header{Name: "usr/bin/other", Typeflag: tar.TypeLink, Linkname: "bin/sh"}, ""},
		{tar.Header{Name: "dev/null", Typeflag: tar.TypeChar}, ""},
	})
	if err := extractRootfs(directory, reader, 1<<20, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"usr/bin/sh", "usr/bin/other"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || string(data) != "shell" {
			t.Fatalf("%s: %q %v", name, data, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(directory, "dev/null")); !os.IsNotExist(err) {
		t.Fatal("device entry should be omitted")
	}
}

func TestRootfsRejectsEscapesAndCapacity(t *testing.T) {
	for _, entries := range [][]tarEntry{
		{{tar.Header{Name: "../../escape", Typeflag: tar.TypeReg}, "x"}},
		{{tar.Header{Name: "/escape", Typeflag: tar.TypeReg}, "x"}},
		{{tar.Header{Name: "dir", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}, ""},
			{tar.Header{Name: "dir/file", Typeflag: tar.TypeReg}, "x"}},
		{{tar.Header{Name: "large", Typeflag: tar.TypeReg}, "larger than the limit"}},
	} {
		if err := extractRootfs(t.TempDir(), archive(t, entries), 4, false, nil); err == nil {
			t.Fatalf("accepted invalid archive %+v", entries)
		}
	}
}

func TestRealRootfsArchive(t *testing.T) {
	path := os.Getenv("SAPPHIRE_TEST_ROOTFS")
	if path == "" {
		t.Skip("set SAPPHIRE_TEST_ROOTFS to exercise a verified real distro tar.gz")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	destination := t.TempDir()
	if err := extractRootfs(destination, reader, 1<<30, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"etc/os-release", "bin/busybox"} {
		if _, err := os.Stat(filepath.Join(destination, path)); err != nil {
			t.Fatal(err)
		}
	}
}
