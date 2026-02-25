package ringbuffer

import (
	"bytes"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

func TestPushPop(t *testing.T) {
	rb := New(8, 64)
	data := []byte("hello world")
	buf := make([]byte, 64)

	if !rb.Push(data) {
		t.Fatal("Push failed on empty buffer")
	}

	n, ok := rb.Pop(buf)
	if !ok {
		t.Fatal("Pop failed on non-empty buffer")
	}
	if !bytes.Equal(buf[:n], data) {
		t.Fatalf("got %q, want %q", buf[:n], data)
	}
}

func TestFullBuffer(t *testing.T) {
	rb := New(4, 64)
	data := []byte("x")

	for i := 0; i < 4; i++ {
		if !rb.Push(data) {
			t.Fatalf("Push failed at index %d", i)
		}
	}
	if rb.Push(data) {
		t.Fatal("Push should fail on full buffer")
	}
}

func TestEmptyBuffer(t *testing.T) {
	rb := New(4, 64)
	buf := make([]byte, 64)

	_, ok := rb.Pop(buf)
	if ok {
		t.Fatal("Pop should fail on empty buffer")
	}
}

func TestWrapAround(t *testing.T) {
	rb := New(4, 64)
	buf := make([]byte, 64)

	for cycle := 0; cycle < 10; cycle++ {
		for i := 0; i < 4; i++ {
			v := byte(cycle*4 + i)
			if !rb.Push([]byte{v}) {
				t.Fatalf("Push failed at cycle %d, index %d", cycle, i)
			}
		}
		for i := 0; i < 4; i++ {
			n, ok := rb.Pop(buf)
			if !ok {
				t.Fatalf("Pop failed at cycle %d, index %d", cycle, i)
			}
			want := byte(cycle*4 + i)
			if n != 1 || buf[0] != want {
				t.Fatalf("cycle %d index %d: got (%d, %d), want (1, %d)", cycle, i, n, buf[0], want)
			}
		}
	}
}

func TestLenCap(t *testing.T) {
	rb := New(5, 64) // rounds to 8
	if rb.Cap() != 8 {
		t.Fatalf("Cap: got %d, want 8", rb.Cap())
	}
	if rb.SlotSize() != 64 {
		t.Fatalf("SlotSize: got %d, want 64", rb.SlotSize())
	}

	for i := 0; i < 5; i++ {
		rb.Push([]byte{byte(i)})
	}
	if rb.Len() != 5 {
		t.Fatalf("Len: got %d, want 5", rb.Len())
	}
}

func TestPopBatch(t *testing.T) {
	rb := New(8, 64)

	for i := 0; i < 5; i++ {
		if !rb.Push([]byte{byte(i)}) {
			t.Fatalf("Push failed at %d", i)
		}
	}

	bufs := make([][]byte, 3)
	for i := range bufs {
		bufs[i] = make([]byte, 64)
	}

	n := rb.PopBatch(bufs)
	if n != 3 {
		t.Fatalf("PopBatch(3): got %d items", n)
	}
	for i := 0; i < 3; i++ {
		if len(bufs[i]) != 1 || bufs[i][0] != byte(i) {
			t.Fatalf("PopBatch item %d: got %v, want [%d]", i, bufs[i], i)
		}
	}

	bufs2 := make([][]byte, 5)
	for i := range bufs2 {
		bufs2[i] = make([]byte, 64)
	}
	n = rb.PopBatch(bufs2)
	if n != 2 {
		t.Fatalf("PopBatch(remaining): got %d items, want 2", n)
	}
	if bufs2[0][0] != 3 || bufs2[1][0] != 4 {
		t.Fatalf("PopBatch remaining: got [%d, %d], want [3, 4]", bufs2[0][0], bufs2[1][0])
	}
}

func TestPopBatchEmpty(t *testing.T) {
	rb := New(8, 64)
	bufs := make([][]byte, 4)
	for i := range bufs {
		bufs[i] = make([]byte, 64)
	}
	if n := rb.PopBatch(bufs); n != 0 {
		t.Fatalf("PopBatch on empty buffer: got %d, want 0", n)
	}
}

func TestSPSCOrdering(t *testing.T) {
	const count = 100_000
	rb := New(1024, 8)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < count; i++ {
			data := []byte{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)}
			for !rb.Push(data) {
				runtime.Gosched()
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 8)
		for i := 0; i < count; i++ {
			for {
				n, ok := rb.Pop(buf)
				if ok {
					want := []byte{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)}
					if n != 4 || !bytes.Equal(buf[:n], want) {
						t.Errorf("index %d: got %v, want %v", i, buf[:n], want)
						return
					}
					break
				}
				runtime.Gosched()
			}
		}
	}()

	wg.Wait()
}

func TestMPMCCompleteness(t *testing.T) {
	const (
		numProducers = 4
		numConsumers = 4
		perProducer  = 10_000
		total        = numProducers * perProducer
	)

	rb := New(1024, 8)
	var consumed atomic.Uint64
	var wg sync.WaitGroup

	for p := 0; p < numProducers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := make([]byte, 8)
			for i := 0; i < perProducer; i++ {
				for !rb.Push(data) {
					runtime.Gosched()
				}
			}
		}()
	}

	for c := 0; c < numConsumers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 8)
			for consumed.Load() < total {
				if _, ok := rb.Pop(buf); ok {
					consumed.Add(1)
				} else {
					runtime.Gosched()
				}
			}
		}()
	}

	wg.Wait()

	if got := consumed.Load(); got != total {
		t.Fatalf("consumed %d, want %d", got, total)
	}
}

func TestNextPowerOf2(t *testing.T) {
	cases := []struct{ in, want uint64 }{
		{0, 1}, {1, 1}, {2, 2}, {3, 4}, {4, 4},
		{5, 8}, {7, 8}, {8, 8}, {9, 16}, {1023, 1024}, {1024, 1024},
	}
	for _, tc := range cases {
		if got := nextPowerOf2(tc.in); got != tc.want {
			t.Errorf("nextPowerOf2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

