package envexec

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"

	"github.com/criyle/go-sandbox/pkg/memfd"
)

const memfdName = "input"

var enableMemFd atomic.Int32

func readerToFile(reader io.Reader) (*os.File, error) {
	if enableMemFd.Load() == 0 {
		f, err := memfd.DupToMemfd(memfdName, reader)
		if err == nil {
			return f, err
		}
		enableMemFd.Store(1)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	go func() {
		defer w.Close()
		w.ReadFrom(reader)
	}()
	return r, nil
}

func copyDir(src *os.File, dst string) error {
	// make sure dir exists
	if err := os.MkdirAll(dst, 0777); err != nil {
		return err
	}
	newDir, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer newDir.Close()

	dir := src
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := copyFileDir(int(dir.Fd()), int(newDir.Fd()), n); err != nil {
			return err
		}
	}
	return nil
}

func copyFileDir(srcDirFd, dstDirFd int, name string) error {
	// open the source file or directory
	fd, err := syscall.Openat(srcDirFd, name, syscall.O_CLOEXEC|syscall.O_RDONLY, 0777)
	if err != nil {
		return fmt.Errorf("copyfiledir: openat src %q: %w", name, err)
	}
	defer syscall.Close(fd)

	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("copyfiledir: fstat %q: %w", name, err)
	}

	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		// open the dst file
		dstFd, err := syscall.Openat(dstDirFd, name, syscall.O_CLOEXEC|syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC, 0777)
		if err != nil {
			return fmt.Errorf("copyfiledir: openat dst %q: %w", name, err)
		}
		defer syscall.Close(dstFd)

		// send file
		if _, err := syscall.Sendfile(dstFd, fd, nil, int(st.Size)); err != nil && err != syscall.EINVAL {
			return fmt.Errorf("copyfiledir: sendfile %q: %w", name, err)
		}
		return nil
	case syscall.S_IFDIR:
		// create the directory in the destination
		if err := syscall.Mkdirat(dstDirFd, name, 0777); err != nil && !os.IsExist(err) {
			return fmt.Errorf("copyfiledir: mkdirat dst %q: %w", name, err)
		}
		// open the new directory in the destination
		newDstFd, err := syscall.Openat(dstDirFd, name, syscall.O_CLOEXEC|syscall.O_RDONLY, 0777)
		if err != nil {
			return fmt.Errorf("copyfiledir: openat dst dir %q: %w", name, err)
		}
		defer syscall.Close(newDstFd)

		// read entries in the source directory
		srcDir := os.NewFile(uintptr(fd), name)
		names, err := srcDir.Readdirnames(-1)
		if err != nil {
			return fmt.Errorf("copyfiledir: readdirnames %q: %w", name, err)
		}
		for _, n := range names {
			if n == "." || n == ".." {
				continue
			}
			if err := copyFileDir(int(fd), newDstFd, n); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("copyfiledir: %q is not a regular file or directory", name)
	}
}
