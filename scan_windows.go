//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"syscall"
)

// readDirFast reads a directory with a single Win32 FindFirstFile/FindNextFile
// pass. Unlike os.ReadDir it returns the file size (high+low) that the
// directory enumeration already carries, so we never pay a second stat()
// syscall per entry. That single optimisation is most of the speed win.
func readDirFast(dir string) ([]dirEntry, error) {
	pattern := longPath(dir) + `\*`
	p, err := syscall.UTF16PtrFromString(pattern)
	if err != nil {
		return nil, err
	}
	var d syscall.Win32finddata
	h, err := syscall.FindFirstFile(p, &d)
	if err != nil {
		return nil, err
	}
	defer syscall.FindClose(h)

	out := make([]dirEntry, 0, 64)
	for {
		name := syscall.UTF16ToString(d.FileName[:])
		if name != "." && name != ".." {
			attrs := d.FileAttributes
			out = append(out, dirEntry{
				name:    name,
				size:    int64(d.FileSizeHigh)<<32 | int64(d.FileSizeLow),
				isDir:   attrs&syscall.FILE_ATTRIBUTE_DIRECTORY != 0,
				reparse: attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0,
			})
		}
		if err := syscall.FindNextFile(h, &d); err != nil {
			// ERROR_NO_MORE_FILES ends the enumeration cleanly.
			break
		}
	}
	return out, nil
}

// longPath turns C:\foo into \\?\C:\foo so paths past MAX_PATH still enumerate.
func longPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if strings.HasPrefix(p, `\\`) { // UNC: \\server\share
		return `\\?\UNC\` + strings.TrimPrefix(p, `\\`)
	}
	return `\\?\` + p
}

func baseName(p string) string {
	p = strings.TrimRight(p, `\/`)
	if p == "" {
		return p
	}
	if b := filepath.Base(p); b != "." && b != string(filepath.Separator) {
		return b
	}
	return p
}
