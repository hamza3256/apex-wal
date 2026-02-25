package ringbuffer

import (
	"encoding/binary"
	"hash/crc32"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// pinThread locks the calling goroutine to its current OS thread and disables
// GC. This eliminates two sources of latency noise: scheduler-driven thread
// migration (which pollutes L1/L2 caches) and stop-the-world GC pauses.
// Returns a cleanup function that restores GC.
func pinThread() func() {
	runtime.LockOSThread()
	prev := debug.SetGCPercent(-1)
	return func() {
		debug.SetGCPercent(prev)
		runtime.UnlockOSThread()
	}
}

// reportPercentiles feeds pre-recorded nanosecond samples into an HDR
// histogram and reports P50/P90/P99/P99.9 via b.ReportMetric so they
// appear directly in `go test -bench` output.
func reportPercentiles(b *testing.B, samples []int64, count int) {
	b.Helper()
	hist := hdrhistogram.New(1, 1_000_000, 3) // 1 ns .. 1 ms, 3 sig digits
	for i := 0; i < count; i++ {
		_ = hist.RecordValue(samples[i])
	}
	b.ReportMetric(float64(hist.ValueAtQuantile(50)), "ns/p50")
	b.ReportMetric(float64(hist.ValueAtQuantile(90)), "ns/p90")
	b.ReportMetric(float64(hist.ValueAtQuantile(99)), "ns/p99")
	b.ReportMetric(float64(hist.ValueAtQuantile(99.9)), "ns/p99.9")
}

// ---------------------------------------------------------------------------
// Benchmark: Latency Distribution (SPSC, pinned thread)
// ---------------------------------------------------------------------------

func BenchmarkLatencyDistribution(b *testing.B) {
	const (
		bufSlots = 1 << 17 // 131072
		slotSize = 64
	)

	cleanup := pinThread()
	defer cleanup()

	rb := New(bufSlots, slotSize)
	payload := make([]byte, slotSize)

	// Pre-allocate sample buffer for the maximum iteration count.
	// Capped at 16M to avoid excessive memory on very long runs;
	// if b.N exceeds this we simply stop recording after the cap.
	const maxSamples = 1 << 24
	samples := make([]int64, maxSamples)

	// Background consumer: drains the buffer as fast as possible so the
	// producer (which we are measuring) never blocks on a full ring.
	var consumerDone atomic.Bool
	buf := make([]byte, slotSize)
	go func() {
		for !consumerDone.Load() {
			if _, ok := rb.Pop(buf); !ok {
				runtime.Gosched()
			}
		}
		// Drain remaining entries.
		for {
			if _, ok := rb.Pop(buf); !ok {
				return
			}
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()

	sampleCount := b.N
	if sampleCount > maxSamples {
		sampleCount = maxSamples
	}

	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		for !rb.Push(payload) {
			runtime.Gosched()
		}
		if i < sampleCount {
			samples[i] = int64(time.Since(t0))
		}
	}

	b.StopTimer()
	consumerDone.Store(true)

	reportPercentiles(b, samples, sampleCount)
}

// ---------------------------------------------------------------------------
// Benchmark: 1000-Goroutine Hot-Path Contention
// ---------------------------------------------------------------------------

func BenchmarkHotPath1000(b *testing.B) {
	const (
		numProducers = 1000
		bufSlots     = 1 << 17 // 131072
		slotSize     = 64
		batchSize    = 64
	)

	rb := New(bufSlots, slotSize)
	payload := make([]byte, slotSize)

	opsPerProducer := b.N / numProducers
	if opsPerProducer < 1 {
		opsPerProducer = 1
	}
	totalOps := opsPerProducer * numProducers

	// Per-goroutine sample slices: no synchronisation needed in the hot loop.
	const maxPerGoroutine = 1 << 16 // 65536 samples per goroutine
	allSamples := make([][]int64, numProducers)
	sampleCounts := make([]int, numProducers)
	for i := range allSamples {
		cap := opsPerProducer
		if cap > maxPerGoroutine {
			cap = maxPerGoroutine
		}
		allSamples[i] = make([]int64, cap)
	}

	// Single consumer using PopBatch.
	var consumerDone atomic.Bool
	var consumed atomic.Int64
	bufs := make([][]byte, batchSize)
	for i := range bufs {
		bufs[i] = make([]byte, slotSize)
	}
	go func() {
		for !consumerDone.Load() || consumed.Load() < int64(totalOps) {
			for i := range bufs {
				bufs[i] = bufs[i][:cap(bufs[i])]
			}
			n := rb.PopBatch(bufs)
			if n == 0 {
				runtime.Gosched()
				continue
			}
			consumed.Add(int64(n))
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()

	var wg sync.WaitGroup
	wg.Add(numProducers)
	for p := 0; p < numProducers; p++ {
		go func(id int) {
			defer wg.Done()
			sl := allSamples[id]
			cap := len(sl)
			count := 0
			for j := 0; j < opsPerProducer; j++ {
				t0 := time.Now()
				for !rb.Push(payload) {
					runtime.Gosched()
				}
				if count < cap {
					sl[count] = int64(time.Since(t0))
					count++
				}
			}
			sampleCounts[id] = count
		}(p)
	}
	wg.Wait()

	b.StopTimer()
	consumerDone.Store(true)

	// Merge all per-goroutine samples into one histogram.
	hist := hdrhistogram.New(1, 10_000_000, 3) // 1 ns .. 10 ms
	for i := 0; i < numProducers; i++ {
		for j := 0; j < sampleCounts[i]; j++ {
			_ = hist.RecordValue(allSamples[i][j])
		}
	}
	b.ReportMetric(float64(hist.ValueAtQuantile(50)), "ns/p50")
	b.ReportMetric(float64(hist.ValueAtQuantile(90)), "ns/p90")
	b.ReportMetric(float64(hist.ValueAtQuantile(99)), "ns/p99")
	b.ReportMetric(float64(hist.ValueAtQuantile(99.9)), "ns/p99.9")
}

// ---------------------------------------------------------------------------
// Test: Write-Ahead Ordering Verification
// ---------------------------------------------------------------------------

// TestWriteAheadOrdering proves that the ring buffer preserves per-producer
// FIFO ordering under concurrent load: each producer's entries are consumed
// in exactly the order they were pushed, with no loss or duplication.
func TestWriteAheadOrdering(t *testing.T) {
	const (
		numProducers = 8
		perProducer  = 100_000
		total        = numProducers * perProducer
		slotSize     = 8
	)

	rb := New(8192, slotSize)
	var wg sync.WaitGroup

	// Producers: each writes (producerID, sequenceNumber) as two uint32s.
	for p := 0; p < numProducers; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var buf [8]byte
			for seq := 0; seq < perProducer; seq++ {
				binary.LittleEndian.PutUint32(buf[0:4], uint32(id))
				binary.LittleEndian.PutUint32(buf[4:8], uint32(seq))
				for !rb.Push(buf[:]) {
					runtime.Gosched()
				}
			}
		}(p)
	}

	// Consumer: drains all entries, groups by producer.
	perProducerSeqs := make([][]uint32, numProducers)
	for i := range perProducerSeqs {
		perProducerSeqs[i] = make([]uint32, 0, perProducer)
	}

	var consumed int
	readBuf := make([]byte, slotSize)

	// Drain in main test goroutine (single consumer).
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	for consumed < total {
		n, ok := rb.Pop(readBuf)
		if !ok {
			select {
			case <-done:
				// Producers finished; keep draining remaining entries.
				continue
			default:
				runtime.Gosched()
				continue
			}
		}
		if n != slotSize {
			t.Fatalf("entry %d: got %d bytes, want %d", consumed, n, slotSize)
		}
		pid := binary.LittleEndian.Uint32(readBuf[0:4])
		seq := binary.LittleEndian.Uint32(readBuf[4:8])
		perProducerSeqs[pid] = append(perProducerSeqs[pid], seq)
		consumed++
	}

	// Verify per-producer ordering: strictly monotonically increasing.
	for pid := 0; pid < numProducers; pid++ {
		seqs := perProducerSeqs[pid]
		if len(seqs) != perProducer {
			t.Fatalf("producer %d: got %d entries, want %d", pid, len(seqs), perProducer)
		}
		for i := 1; i < len(seqs); i++ {
			if seqs[i] != seqs[i-1]+1 {
				t.Fatalf("producer %d: ordering violation at index %d: seq[%d]=%d, seq[%d]=%d",
					pid, i, i-1, seqs[i-1], i, seqs[i])
			}
		}
		if seqs[0] != 0 || seqs[len(seqs)-1] != uint32(perProducer-1) {
			t.Fatalf("producer %d: expected range [0, %d], got [%d, %d]",
				pid, perProducer-1, seqs[0], seqs[len(seqs)-1])
		}
	}

	t.Logf("PASS: %d entries from %d producers consumed in correct per-producer order", total, numProducers)
}

// ---------------------------------------------------------------------------
// Test: Data Integrity (CRC32 verification)
// ---------------------------------------------------------------------------

// TestDataIntegrity pushes entries with embedded CRC32 checksums and verifies
// zero corruption on the consumer side.
func TestDataIntegrity(t *testing.T) {
	const (
		numEntries = 50_000
		slotSize   = 64
		headerSize = 4 // CRC32 stored in first 4 bytes
	)

	rb := New(8192, slotSize)
	payloadSize := slotSize - headerSize

	// Producer: writes [CRC32(payload) | payload] into each slot.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var entry [slotSize]byte
		for i := 0; i < numEntries; i++ {
			// Fill payload region with a deterministic pattern.
			for j := headerSize; j < slotSize; j++ {
				entry[j] = byte(i + j)
			}
			checksum := crc32.ChecksumIEEE(entry[headerSize:slotSize])
			binary.LittleEndian.PutUint32(entry[0:headerSize], checksum)
			for !rb.Push(entry[:]) {
				runtime.Gosched()
			}
		}
	}()

	// Consumer: pops each entry, recomputes CRC32, asserts match.
	readBuf := make([]byte, slotSize)
	var corrupted int
	for i := 0; i < numEntries; i++ {
		for {
			n, ok := rb.Pop(readBuf)
			if ok {
				if n != slotSize {
					t.Fatalf("entry %d: got %d bytes, want %d", i, n, slotSize)
				}
				storedCRC := binary.LittleEndian.Uint32(readBuf[0:headerSize])
				computedCRC := crc32.ChecksumIEEE(readBuf[headerSize:slotSize])
				if storedCRC != computedCRC {
					corrupted++
					t.Errorf("entry %d: CRC mismatch (stored=0x%08X, computed=0x%08X)",
						i, storedCRC, computedCRC)
				}
				break
			}
			runtime.Gosched()
		}
	}

	wg.Wait()

	if corrupted > 0 {
		t.Fatalf("%d/%d entries corrupted", corrupted, numEntries)
	}
	t.Logf("PASS: %d entries verified with CRC32, zero corruption (%d byte payload)",
		numEntries, payloadSize)
}

// ---------------------------------------------------------------------------
// Test: Quick sanity check for reportPercentiles (avoid unused import)
// ---------------------------------------------------------------------------

func TestHistogramSanity(t *testing.T) {
	hist := hdrhistogram.New(1, 1_000_000, 3)
	for i := int64(1); i <= 1000; i++ {
		if err := hist.RecordValue(i); err != nil {
			t.Fatal(err)
		}
	}
	p50 := hist.ValueAtQuantile(50)
	p99 := hist.ValueAtQuantile(99)
	if p50 < 400 || p50 > 600 {
		t.Fatalf("p50=%d, expected ~500", p50)
	}
	if p99 < 980 || p99 > 1010 {
		t.Fatalf("p99=%d, expected ~990", p99)
	}
}
