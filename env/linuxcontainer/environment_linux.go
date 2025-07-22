package linuxcontainer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/criyle/go-judge/envexec"
	"github.com/criyle/go-sandbox/container"
	"github.com/criyle/go-sandbox/pkg/cgroup"
	"github.com/criyle/go-sandbox/pkg/rlimit"
	"github.com/criyle/go-sandbox/runner"
	"golang.org/x/sys/unix"
)

var _ envexec.Environment = &environ{}

// environ defines interface to access container resources
type environ struct {
	container.Environment
	cgPool  CgroupPool
	wd      *os.File // container work dir
	workDir string
	cpuset  string
	seccomp []syscall.SockFilter
	cpuRate bool
	cgFd    bool
}

// Destroy destroys the environment
func (c *environ) Destroy() error {
	return c.Environment.Destroy()
}

func (c *environ) Reset() error {
	return c.Environment.Reset()
}

// Execve execute process inside the environment
func (c *environ) Execve(ctx context.Context, param envexec.ExecveParam) (envexec.Process, error) {
	var (
		cg       Cgroup
		syncFunc func(int) error
		err      error
		cgFd     uintptr
	)

	limit := param.Limit
	if c.cgPool != nil {
		cg, err = c.cgPool.Get()
		if err != nil {
			return nil, fmt.Errorf("execve: failed to get cgroup: %w", err)
		}
		if err := c.setCgroupLimit(cg, limit); err != nil {
			return nil, err
		}
		if c.cgFd {
			f, err := cg.Open()
			if err != nil {
				return nil, fmt.Errorf("execve: failed to get cgroup fd: %w", err)
			}
			defer f.Close()
			cgFd = f.Fd()
		} else {
			syncFunc = cg.AddProc
		}
	}

	rLimits := rlimit.RLimits{
		CPU:         uint64(limit.Time.Truncate(time.Second)/time.Second) + 1,
		FileSize:    limit.Output.Byte(),
		Stack:       limit.Stack.Byte(),
		OpenFile:    limit.OpenFile,
		DisableCore: true,
	}

	if limit.DataSegment || c.cgPool == nil {
		rLimits.Data = limit.Memory.Byte()
	}
	if limit.AddressSpace {
		rLimits.AddressSpace = limit.Memory.Byte()
	}

	// wait for sync or error before turn (avoid file close before pass to child process)
	syncDone := make(chan struct{})

	p := container.ExecveParam{
		Args:     param.Args,
		Env:      param.Env,
		Files:    param.Files,
		CTTY:     param.TTY,
		ExecFile: param.ExecFile,
		RLimits:  rLimits.PrepareRLimit(),
		Seccomp:  c.seccomp,
		SyncFunc: func(pid int) error {
			defer close(syncDone)
			if syncFunc != nil {
				return syncFunc(pid)
			}
			return nil
		},
		SyncAfterExec: syncFunc == nil,
		CgroupFD:      cgFd,
	}
	proc := newProcess(func() runner.Result {
		return c.Environment.Execve(ctx, p)
	}, cg, c.cgPool)

	select {
	case <-proc.done:
	case <-syncDone:
	}

	return proc, nil
}

// WorkDir returns opened work directory, should not close after
func (c *environ) WorkDir() *os.File {
	c.wd.Seek(0, 0)
	return c.wd
}

// Open opens file relative to work directory
func (c *environ) Open(path string, flags int, perm os.FileMode) (*os.File, error) {
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(c.workDir, path)
		if err != nil {
			return nil, fmt.Errorf("openatworkdir: %w", err)
		}
	}
	fd, err := syscall.Openat(int(c.wd.Fd()), path, flags|syscall.O_CLOEXEC, uint32(perm))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		return nil, fmt.Errorf("openatworkdir: failed to create file")
	}
	return f, nil
}

func (c *environ) MkWorkDir() error {
	// Create the work directory with 0755 permissions
	err := syscall.Mkdirat(int(c.wd.Fd()), ".", 0755)
	if err != nil {
		// Check if directory already exists
		var stat unix.Stat_t
		err1 := unix.Fstatat(int(c.wd.Fd()), ".", &stat, 0)
		if err1 == nil && stat.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			return nil // Directory already exists, that's fine
		}
		return &os.PathError{Op: "mkdir", Path: c.workDir, Err: err}
	}
	return nil
}

// MkdirAll equivalent to os.MkdirAll but in container
func (c *environ) MkdirAll(path string, perm os.FileMode) error {
	if path == "" || path == "." {
		return nil
	}
	if filepath.IsAbs(path) {
		r, err := filepath.Rel(c.workDir, path)
		if err != nil {
			return &os.PathError{Op: "mkdir", Path: path, Err: syscall.EINVAL}
		}
		return c.MkdirAll(r, perm)
	}
	// fast path
	wd := int(c.wd.Fd())
	var stat unix.Stat_t
	err := unix.Fstatat(wd, path, &stat, 0)
	if err == nil {
		if stat.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}
	// slow path
	// Slow path: make sure parent exists and then call Mkdir for path.
	i := len(path)
	for i > 0 && os.IsPathSeparator(path[i-1]) { // Skip trailing path separator.
		i--
	}

	j := i
	for j > 0 && !os.IsPathSeparator(path[j-1]) { // Scan backward over element.
		j--
	}

	if j > 1 {
		// Create parent.
		err = c.MkdirAll(path[:j-1], perm)
		if err != nil {
			return err
		}
	}
	err = syscall.Mkdirat(wd, path, uint32(perm.Perm()))
	if err != nil {
		err1 := unix.Fstatat(wd, path, &stat, 0)
		if err1 == nil && stat.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			return nil
		}
		return err
	}
	return nil
}

func (c *environ) Symlink(oldName, newName string) error {
	var err error
	if filepath.IsAbs(newName) {
		newName, err = filepath.Rel(c.workDir, newName)
		if err != nil {
			return &os.PathError{Op: "symlink", Path: newName, Err: syscall.EINVAL}
		}
	}
	if filepath.IsAbs(oldName) {
		oldName, err = filepath.Rel(c.workDir, oldName)
		if err != nil {
			return &os.PathError{Op: "symlink", Path: oldName, Err: syscall.EINVAL}
		}
	}
	return unix.Symlinkat(oldName, int(c.wd.Fd()), newName)
}

// func (c *environ) HardLink(srcDir, dstDir string) error {
// 	// Walk srcDir recursively
// 	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
// 		if err != nil {
// 			return err
// 		}
// 		rel, err := filepath.Rel(srcDir, path)
// 		if err != nil {
// 			return err
// 		}
// 		if rel == "." {
// 			// skip root
// 			return nil
// 		}
// 		srcAbs := path                       // always absolute on host
// 		dstRel := filepath.Join(dstDir, rel) // relative to container workdir

// 		info, err := d.Info()
// 		if err != nil {
// 			return err
// 		}

// 		switch {
// 		case d.IsDir():
// 			// Create directory in dstDir
// 			if err := c.MkdirAll(dstRel, info.Mode().Perm()); err != nil {
// 				return err
// 			}
// 		case (info.Mode() & os.ModeSymlink) != 0:
// 			// Skip symlinks
// 			return nil
// 		case info.Mode().IsRegular():
// 			// Create hard link for regular file
// 			err := unix.Linkat(unix.AT_FDCWD, srcAbs, int(c.wd.Fd()), dstRel, 0)
// 			if err != nil {
// 				return &os.LinkError{Op: "link", Old: srcAbs, New: dstRel, Err: err}
// 			}
// 		default:
// 			// Skip other special files
// 			return nil
// 		}
// 		return nil
// 	})
// }

func (c *environ) setCgroupLimit(cg Cgroup, limit envexec.Limit) error {
	cpuSet := limit.CPUSet
	if cpuSet == "" {
		cpuSet = c.cpuset
	}
	if cpuSet != "" {
		if err := cg.SetCpuset(cpuSet); isCgroupSetHasError(err) {
			return fmt.Errorf("execve: cgroup: failed to set cpuset limit: %w", err)
		}
	}
	if c.cpuRate && limit.Rate > 0 {
		if err := cg.SetCPURate(limit.Rate); isCgroupSetHasError(err) {
			return fmt.Errorf("execve: cgroup: failed to set cpu rate limit: %w", err)
		}
	}
	if err := cg.SetMemoryLimit(limit.Memory); isCgroupSetHasError(err) {
		return fmt.Errorf("execve: cgroup: failed to set memory limit: %w", err)
	}
	if err := cg.SetProcLimit(limit.Proc); isCgroupSetHasError(err) {
		return fmt.Errorf("execve: cgroup: failed to set process limit: %w", err)
	}
	return nil
}

func isCgroupSetHasError(err error) bool {
	return err != nil && !errors.Is(err, cgroup.ErrNotInitialized) && !errors.Is(err, os.ErrNotExist)
}

func (c *environ) CopyDir(src, dst string) error {
	srcDir, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcDir.Close()

	return c.copyDir(srcDir, dst)
}

func (c *environ) copyDir(src *os.File, dst string) error {
	// make sure dir exists
	if err := c.MkdirAll(dst, 0777); err != nil {
		return err
	}
	newDir, err := syscall.Openat(int(c.wd.Fd()), dst, syscall.O_CLOEXEC|syscall.O_RDONLY, 0777)
	if err != nil {
		return err
	}
	defer syscall.Close(newDir)

	dir := src
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := copyFileDir(int(dir.Fd()), newDir, n); err != nil {
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
