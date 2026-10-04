package session

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// moveAside moves a file or directory tree to destination, which must not
// exist yet. Across filesystems it copies the tree and removes the source only
// once the copy is complete; a failed copy is removed and the source kept.
func moveAside(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("%s already exists", destination)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	err := os.Rename(source, destination)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyTree(source, destination); err != nil {
		_ = os.RemoveAll(destination)
		return fmt.Errorf("copy %s aside: %w", source, err)
	}
	return os.RemoveAll(source)
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			if err := copyFile(path, target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(target, info.ModTime(), info.ModTime())
		default:
			return nil // sockets and devices are not evidence
		}
	})
}

func copyFile(source, destination string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
