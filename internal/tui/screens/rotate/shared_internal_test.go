package rotate

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A file swapped for a FIFO after its Stat must not hang the read: the open
// never blocks, and the opened file is refused as not regular.
func TestOpenValueNeverBlocks(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "value")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no FIFO here:", err)
	}
	done := make(chan string, 1)
	go func() {
		f, err := openValue(fifo)
		if err == nil {
			f.Close()
			done <- "opened"
			return
		}
		done <- "refused"
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}
