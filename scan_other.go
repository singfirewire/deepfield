//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
)

type statT = os.FileInfo

func readDirFast(dir string) ([]dirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	list, err := f.Readdir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]dirEntry, 0, len(list))
	for _, fi := range list {
		out = append(out, dirEntry{
			name:    fi.Name(),
			size:    fi.Size(),
			isDir:   fi.IsDir(),
			reparse: fi.Mode()&os.ModeSymlink != 0,
		})
	}
	return out, nil
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if b := filepath.Base(p); b != "." && b != "/" {
		return b
	}
	return filepath.VolumeName(p)
}
