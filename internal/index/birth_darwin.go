package index

import (
	"os"
	"syscall"
)

// birth returns a file's creation time in unix ms (0 when unknown). A copy of
// a transcript is always created after the original, so the earlier-born file
// owns the responses both contain.
func birth(_ string, fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Birthtimespec.Sec*1000 + st.Birthtimespec.Nsec/1_000_000
	}
	return 0
}
