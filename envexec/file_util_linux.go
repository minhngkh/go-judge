package envexec

import (
	   "fmt"
	   "io"
	   "os"
	   "path/filepath"
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
	   entries, err := src.Readdir(-1)
	   if err != nil {
			   return err
	   }
	   for _, entry := range entries {
			   name := entry.Name()
			   if entry.IsDir() {
					   // open the subdirectory in src
					   subSrc, err := os.Open(filepath.Join(src.Name(), name))
					   if err != nil {
							   return err
					   }
					   defer subSrc.Close()
					   // create the subdirectory in dst
					   subDst := filepath.Join(dst, name)
					   if err := copyDir(subSrc, subDst); err != nil {
							   return err
					   }
			   } else if entry.Mode().IsRegular() {
					   if err := copyFileDir(int(src.Fd()), -1, name, dst); err != nil {
							   return err
					   }
			   }
	   }
	   return nil
}

func copyFileDir(srcDirFd, _ int, name string, dstDir string) error {
	   // open the source file
	   fd, err := syscall.Openat(srcDirFd, name, syscall.O_CLOEXEC|syscall.O_RDONLY, 0777)
	   if err != nil {
			   return fmt.Errorf("copyfiledir: openat src %q: %w", name, err)
	   }
	   defer syscall.Close(fd)

	   var st syscall.Stat_t
	   if err := syscall.Fstat(fd, &st); err != nil {
			   return fmt.Errorf("copyfiledir: fstat %q: %w", name, err)
	   }
	   if st.Mode&syscall.S_IFREG == 0 {
			   return fmt.Errorf("copyfiledir: %q is not a regular file", name)
	   }

	   // open the dst file (create in dstDir)
	   dstPath := filepath.Join(dstDir, name)
	   dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0777)
	   if err != nil {
			   return fmt.Errorf("copyfiledir: open dst %q: %w", dstPath, err)
	   }
	   defer dstFile.Close()

	   srcFile := os.NewFile(uintptr(fd), name)
	   if srcFile == nil {
			   return fmt.Errorf("copyfiledir: failed to create srcFile for %q", name)
	   }
	   defer srcFile.Close()

	   if _, err := io.Copy(dstFile, srcFile); err != nil {
			   return fmt.Errorf("copyfiledir: copy %q: %w", name, err)
	   }
	   return nil
}
