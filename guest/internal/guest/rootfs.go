package guest

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

func checkZeroReader(reader io.Reader, size int64) error {
	buffer := make([]byte, 1<<20)
	for offset := int64(0); offset < size; {
		length := min(int64(len(buffer)), size-offset)
		if _, err := io.ReadFull(reader, buffer[:length]); err != nil {
			return fmt.Errorf("blank disk read at %d: %w", offset, err)
		}
		if bytes.Count(buffer[:length], []byte{0}) != int(length) {
			return fmt.Errorf("data disk contains nonzero data at or after offset %d; refusing to format", offset)
		}
		offset += length
	}
	return nil
}

func archivePath(name string) (string, error) {
	if strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("archive path escapes root: %q", name)
		}
	}
	return path.Clean(name), nil
}

// Interpret absolute symlinks relative to the future root, while keeping all
// actual I/O beneath os.Root. This also handles merged-/usr distro archives.
func resolveInside(root *os.Root, name string) (string, error) {
	pending := strings.Split(strings.TrimPrefix(path.Clean("/"+name), "/"), "/")
	var resolved []string
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(resolved) == 0 {
				return "", errors.New("symlink escapes root")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := strings.Join(append(append([]string{}, resolved...), part), "/")
		target, err := root.Readlink(candidate)
		if err != nil {
			resolved = append(resolved, part)
			continue
		}
		links++
		if links > 40 {
			return "", errors.New("too many rootfs symlinks")
		}
		if strings.HasPrefix(target, "/") {
			resolved = nil
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	if len(resolved) == 0 {
		return ".", nil
	}
	return strings.Join(resolved, "/"), nil
}

func tarMode(mode int64) os.FileMode {
	result := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		result |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		result |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		result |= os.ModeSticky
	}
	return result
}

func extractRootfs(destination string, source io.Reader, maximum int64, ownership bool, metadata func(*os.File, *tar.Header) error) error {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(source)
	var size int64
	var directories []*tar.Header
	var hardlinks []*tar.Header
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read rootfs archive: %w", err)
		}
		name, err := archivePath(header.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		top := strings.Split(name, "/")[0]
		if top == "dev" || top == "proc" || top == "sys" || top == "run" {
			continue
		}
		if header.Size < 0 || header.Size > maximum-size {
			return errors.New("rootfs archive exceeds configured disk capacity")
		}
		size += header.Size
		parent, err := resolveInside(root, path.Dir(name))
		if err != nil {
			return err
		}
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		target := path.Join(parent, path.Base(name))
		switch header.Typeflag {
		case tar.TypeDir:
			target, err = resolveInside(root, name)
			if err == nil {
				err = root.MkdirAll(target, 0o755)
			}
			if err != nil {
				return err
			}
			copy := *header
			copy.Name = target
			directories = append(directories, &copy)
		case tar.TypeReg, tar.TypeRegA:
			if info, statErr := root.Lstat(target); statErr == nil && !info.Mode().IsRegular() {
				return fmt.Errorf("refusing rootfs file over non-regular entry %s", name)
			}
			file, err := root.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tr, header.Size)
			if copyErr == nil && ownership {
				copyErr = file.Chown(header.Uid, header.Gid)
			}
			if copyErr == nil {
				copyErr = file.Chmod(tarMode(header.Mode))
			}
			if copyErr == nil && metadata != nil {
				copyErr = metadata(file, header)
			}
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			if err := root.Chtimes(target, header.ModTime, header.ModTime); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if strings.ContainsRune(header.Linkname, 0) {
				return errors.New("symlink contains NUL")
			}
			if _, err := root.Lstat(target); err == nil {
				if err := root.Remove(target); err != nil {
					return err
				}
			}
			if err := root.Symlink(header.Linkname, target); err != nil {
				return err
			}
			if ownership {
				if err := root.Lchown(target, header.Uid, header.Gid); err != nil {
					return err
				}
			}
		case tar.TypeLink:
			copy := *header
			copy.Name = target
			hardlinks = append(hardlinks, &copy)
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("unsupported rootfs archive entry %q (type %d)", name, header.Typeflag)
		}
	}
	for _, header := range hardlinks {
		name, err := archivePath(header.Linkname)
		if err != nil {
			return err
		}
		source, err := resolveInside(root, name)
		if err != nil {
			return err
		}
		info, err := root.Lstat(source)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("hardlink source is not a regular file: %s", name)
		}
		if err := root.Link(source, header.Name); err != nil {
			return err
		}
	}
	for i := len(directories) - 1; i >= 0; i-- {
		header := directories[i]
		if ownership {
			if err := root.Chown(header.Name, header.Uid, header.Gid); err != nil {
				return err
			}
		}
		if err := root.Chmod(header.Name, tarMode(header.Mode)); err != nil {
			return err
		}
		mtime := header.ModTime
		if mtime.IsZero() {
			mtime = time.Unix(0, 0)
		}
		if err := root.Chtimes(header.Name, mtime, mtime); err != nil {
			return err
		}
	}
	return nil
}
