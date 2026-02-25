# Apex-WAL

A high-performance, lock-free Write-Ahead Log (WAL) core in Go, targeting **sub-10 microsecond P99 latency** and **zero heap allocations** on the hot path.

Apex-WAL's internal messaging bus is a bounded MPMC (multi-producer, multi-consumer) ring buffer based on [Dmitry Vyukov's bounded queue algorithm](https://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue) -- the same design that underpins the LMAX Disruptor.

## Highlights

- **Lock-free** -- all pointer movements use `sync/atomic` CAS. No mutexes, no channels.
- **Zero allocation** -- all slot buffers are pre-allocated at init. The hot path creates no garbage.
- **Cache-line padded** -- head and tail pointers are isolated on separate 64-byte cache lines to eliminate false sharing.
- **Bulk consumption** -- `PopBatch` claims multiple slots in a single CAS, amortising atomic overhead for the persistence layer.
- **HFT-grade benchmarks** -- nanosecond-resolution latency tracking with [HDR Histogram](https://github.com/HdrHistogram/hdrhistogram-go), reporting P50/P90/P99/P99.9 directly in `go test` output.
- **Correctness verified** -- per-producer ordering (write-ahead guarantee) and CRC32 data integrity checked under concurrent load with the race detector.

## Architecture

```
                  ┌─────────────────────────────────────────┐
                  │             RingBuffer                   │
                  │                                         │
   Producers      │  ┌──────┐  ┌──────┐       ┌──────┐     │     Consumers
  (Network)       │  │Slot 0│  │Slot 1│  ...  │Slot N│     │    (Disk I/O)
                  │  └──┬───┘  └──┬───┘       └──┬───┘     │
  goroutine 1 ──CAS──▶ head                      │         │
  goroutine 2 ──CAS──▶ (atomic)                  │         │
  goroutine N ──CAS──▶                       tail ◀──CAS── writer 1
                  │                          (atomic)◀──CAS── writer M
                  └─────────────────────────────────────────┘

  head & tail sit on separate 64B cache lines (no false sharing)
  Each slot: [sequence | dataLen | pad | data | pad] = 128 bytes
```

### Memory Layout

```
Offset  Field
──────  ──────────────────────────
0       head      (atomic.Uint64, 8B)
8       _padH     ([56]byte)             ← pads to 64B cache line
64      tail      (atomic.Uint64, 8B)
72      _padT     ([56]byte)             ← pads to 64B cache line
128     mask, size, slotSize, slots      ← read-only after init
```

## How the Sequence Barrier Works

Each slot carries an atomic `sequence` number that encodes a three-state protocol:

1. **Free** (`seq == pos`): the producer may CAS-advance `head` and claim the slot.
2. **Published** (`seq == pos + 1`): the producer's atomic store acts as a release barrier, making all data writes visible to any consumer that loads the sequence.
3. **Reclaimed** (`seq == pos + bufferSize`): the consumer resets the slot for the next wrap-around cycle.

This per-slot sequencing creates a lock-free **happens-before** relationship between writes and reads. If the producer sees `seq < pos`, the buffer is full -- no overwrite is possible.

## Benchmark Results

Measured on Apple M4 Max (arm64), Go 1.24, 64-byte payloads:

### Throughput (ns/op)

| Benchmark | RingBuffer | Channel | Allocs |
|---|---|---|---|
| SPSC | ~25 ns | ~22 ns | 0 |
| MPMC (4x4) | ~54 ns | ~26 ns | 0 |
| Batch Pop | ~31 ns | -- | 0 |

### Latency Distribution (SPSC, pinned thread, GC disabled)

| Percentile | Latency |
|---|---|
| P50 | 42 ns |
| P90 | 84 ns |
| P99 | 167 ns |
| P99.9 | 291 ns |

**P99 = 167 ns** -- well under the 10,000 ns (10 us) target.

### 1000-Goroutine Contention Stress

| Percentile | Latency |
|---|---|
| P50 | 83 ns |
| P90 | 457 us |
| P99 | 1.6 ms |
| P99.9 | 2.8 ms |

P50 stays at 83 ns even under extreme contention. Tail latencies are dominated by Go scheduler pressure from 1000 goroutines competing for CAS slots.

## API

```go
// Create a ring buffer with 8192 slots, each 64 bytes.
rb := ringbuffer.New(8192, 64)

// Non-blocking push. Returns false if buffer is full.
ok := rb.Push(data)

// Non-blocking pop into pre-allocated buffer.
n, ok := rb.Pop(buf)

// Bulk pop: claims up to len(bufs) entries in a single CAS.
count := rb.PopBatch(bufs)

// Diagnostics.
rb.Len()      // approximate fill level
rb.Cap()      // total slots (power of two)
rb.SlotSize() // max bytes per slot
```

## Running

```bash
# Unit tests (with race detector)
go test -v -race ./ringbuffer/

# Throughput benchmarks
go test -bench='RingBuffer|Channel' -benchmem -count=3 ./ringbuffer/

# Latency distribution (pinned thread, GC disabled)
go test -bench=BenchmarkLatencyDistribution -benchtime=5s -count=3 ./ringbuffer/

# 1000-goroutine stress test
go test -bench=BenchmarkHotPath1000 -benchtime=5s ./ringbuffer/

# Correctness: ordering + CRC32 integrity
go test -run 'TestWriteAheadOrdering|TestDataIntegrity' -v -race ./ringbuffer/
```

## Project Structure

```
apex-wal/
├── go.mod                          # module github.com/hamza3256/apex-wal
├── go.sum
├── README.md
└── ringbuffer/
    ├── ringbuffer.go               # RingBuffer + Slot structs, New, Push, Pop, PopBatch
    ├── ringbuffer_test.go          # unit tests + throughput benchmarks vs channel
    └── bench_test.go               # HFT-grade latency benchmarks + verification suite
```

## Requirements

- Go 1.24+
- No CGo, no unsafe

## License

MIT
