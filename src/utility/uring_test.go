//go:build linux

package utility

import (
	"os"
	"testing"
)

// TestUringWriter verifies the raw io_uring batched writer submits and completes
// a batch of writes correctly to a stream fd (pipe).
func TestUringWriter(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	uw, err := newUringWriter(int(w.Fd()), 16, 4096)
	if err != nil {
		t.Skipf("io_uring unavailable: %v", err)
	}
	defer uw.Close()

	msgs := []string{"hello", "world", "foobar", "baz", "quux"}
	want := ""
	for _, m := range msgs {
		uw.Submit(int(w.Fd()), []byte(m))
		want += m
	}
	if fails := uw.Flush(); fails != 0 {
		t.Fatalf("Flush reported %d failed writes", fails)
	}

	buf := make([]byte, 256)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	// second batch on the same ring (exercises wraparound of head/tail)
	uw.Submit(int(w.Fd()), []byte("again"))
	if fails := uw.Flush(); fails != 0 {
		t.Fatalf("batch2 fails=%d", fails)
	}
	n, _ = r.Read(buf)
	if got := string(buf[:n]); got != "again" {
		t.Fatalf("batch2 got %q want %q", got, "again")
	}
}
