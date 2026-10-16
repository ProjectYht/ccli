//go:build !darwin && !linux

package index

import "os"

// birth is unknown on this platform; the file's first line time decides.
func birth(string, os.FileInfo) int64 { return 0 }
