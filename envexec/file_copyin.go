package envexec

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sync/errgroup"
)

// copyIn copied file from host to container in parallel
func copyIn(m Environment, copyIn map[string]File) ([]FileError, error) {
	var (
		g         errgroup.Group
		fileError []FileError
		l         sync.Mutex
	)
	addError := func(e FileError) {
		l.Lock()
		defer l.Unlock()
		fileError = append(fileError, e)
	}
	for n, f := range copyIn {

		n, f := n, f
		g.Go(func() (err error) {
			t := ErrCopyInOpenFile
			defer func() {
				if err != nil {
					addError(FileError{
						Name:    n,
						Type:    t,
						Message: err.Error(),
					})
				}
			}()

			// Temporary patch to support
			temp, ok := f.(*FileInput)
			if ok {
				fmt.Println(m.WorkDir().Name())

				info, err := os.Stat(temp.Path)
				if err != nil {
					t = ErrCopyInUnknownFile
					return fmt.Errorf("copyin: stat file %q: %w", temp.Path, err)
				}

				// if info.IsDir() {
				// 	// Copy contents of the directory, not the directory itself
				// 	entries, err := os.ReadDir(temp.Path)
				// 	if err != nil {
				// 		t = ErrCopyInUnknownFile
				// 		return fmt.Errorf("copyin: read dir %q: %w", temp.Path, err)
				// 	}

				// 	for _, entry := range entries {
				// 		srcPath := filepath.Join(temp.Path, entry.Name())
				// 		destName := entry.Name()

				// 		if entry.IsDir() {
				// 			// For directories, use CopyFS with proper destination
				// 			destPath := filepath.Join(n, destName)
				// 			err = os.CopyFS(destPath, os.DirFS(srcPath))
				// 		}

				// 		if err != nil {
				// 			t = ErrCopyInUnknownFile
				// 			return fmt.Errorf("copyin: copy %q to %q: %w", srcPath, destName, err)
				// 		}
				// 	}
				// 	fmt.Printf("copyin: copy dir contents from %q to %q\n", temp.Path, m.WorkDir().Name())
				// 	return nil
				// }

				// TODO: no idea why this doesn't work
				if info.IsDir() {
					// Use your syscall-based copyDir to copy the directory recursively
					err = m.CopyDir(temp.Path, n)
					if err != nil {
						t = ErrCopyInUnknownFile
						return fmt.Errorf("copyin: copy dir %q to %q: %w", temp.Path, n, err)
					}
					fmt.Printf("copyin: copy dir from %q to %q\n", temp.Path, n)
					return nil
				}
			}

			hf, err := FileToReader(f)
			if err != nil {
				return fmt.Errorf("copyin: file to reader: %w", err)
			}
			defer hf.Close()

			// ensure path exists
			if err := m.MkdirAll(filepath.Dir(n), 0777); err != nil {
				t = ErrCopyInCreateDir
				return fmt.Errorf("copyin: create dir %q: %w", filepath.Dir(n), err)
			}
			cf, err := m.Open(n, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0777)
			if err != nil {
				t = ErrCopyInCreateFile
				return fmt.Errorf("copyin: open file %q: %w", n, err)
			}
			defer cf.Close()

			_, err = io.Copy(cf, hf)
			if err != nil {
				t = ErrCopyInCopyContent
				return fmt.Errorf("copyin: copy content: %w", err)
			}
			return nil
		})
	}
	return fileError, g.Wait()
}

func symlink(m Environment, symlinks map[string]string) (*FileError, error) {
	for k, v := range symlinks {
		if err := m.Symlink(v, k); err != nil {
			return &FileError{
				Name:    k,
				Type:    ErrSymlink,
				Message: err.Error(),
			}, fmt.Errorf("symlink: %q -> %q: %w", k, v, err)
		}
	}
	return nil, nil
}

// copyFileToEnv copies a single file from host to environment using Environment methods
func copyFileToEnv(m Environment, srcPath, destName string) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Use Environment's Open method to create the file in the sandbox
	destFile, err := m.Open(destName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0777)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, srcFile)
	return err
}
