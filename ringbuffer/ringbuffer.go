package ringbuffer

import (
	"runtime"
	"sync/atomic"
)

const CacheLineSize = 64

// slot holds a single entry in the ring buffer. Each slot is padded to 128 bytes
// (two cache lines) so that adjacent slots' sequence fields never share a cache line.
//
// Layout:
//
//	[0..7]    sequence (atomic, hot — CAS'd by producers and consumers)
//	[8..11]   dataLen  (written by producer, read by consumer; protected by sequence barrier)
//	[12..63]  padding  (isolates sequence from data header)
//	[64..87]  data slice header (read-only after init — ptr, len, cap)
//	[88..127] padding  (isolates this slot from the next slot's sequence)
type slot struct {
	sequence atomic.Uint64
	dataLen  uint32
	_pad0    [CacheLineSize - 12]byte
	data     []byte
	_pad1    [CacheLineSize - 24]byte
}

// RingBuffer is a lock-free, bounded, multi-producer multi-consumer circular queue.
// It uses Dmitry Vyukov's bounded MPMC algorithm where each slot carries a sequence
// number that acts as both a publication flag and a reclamation signal.
//
// head and tail are each isolated on their own cache line to prevent false sharing
// between producers (who CAS head) and consumers (who CAS tail).
type RingBuffer struct {
	head  atomic.Uint64
	_padH [CacheLineSize - 8]byte

	tail  atomic.Uint64
	_padT [CacheLineSize - 8]byte

	mask     uint64
	size     uint64
	slotSize uint64
	slots    []slot
}

// New creates a RingBuffer with the given number of slots and per-slot byte capacity.
// size is rounded up to the next power of two to enable bitwise masking instead of
// modulo division. All slot buffers are pre-allocated; no heap allocation occurs on
// the Push/Pop hot path.
func New(size, slotSize uint64) *RingBuffer {
	size = nextPowerOf2(size)
	rb := &RingBuffer{
		mask:     size - 1,
		size:     size,
		slotSize: slotSize,
		slots:    make([]slot, size),
	}
	for i := uint64(0); i < size; i++ {
		rb.slots[i].sequence.Store(i)
		rb.slots[i].data = make([]byte, slotSize)
	}
	return rb
}

// Push enqueues data into the ring buffer. It is non-blocking: returns false
// immediately if the buffer is full. Safe for concurrent use by multiple producers.
//
// The caller's data is copied into a pre-allocated slot buffer, so the caller
// retains ownership of the source slice. Data exceeding SlotSize is silently truncated.
func (rb *RingBuffer) Push(data []byte) bool {
	for {
		pos := rb.head.Load()
		s := &rb.slots[pos&rb.mask]
		seq := s.sequence.Load()
		diff := int64(seq) - int64(pos)

		switch {
		case diff == 0:
			// Slot is free at this sequence — try to claim it.
			if rb.head.CompareAndSwap(pos, pos+1) {
				n := copy(s.data, data)
				s.dataLen = uint32(n)
				// Publication barrier: makes dataLen and data writes visible
				// to any consumer that observes this Store via sequence.Load().
				s.sequence.Store(pos + 1)
				return true
			}
		case diff < 0:
			// The slot's sequence is behind our position — the consumer hasn't
			// reclaimed it yet. The buffer is full.
			return false
		}
		// diff > 0: another producer already claimed this position; retry with
		// a fresh head load.
		runtime.Gosched()
	}
}

// Pop dequeues the next available entry into buf. It is non-blocking: returns
// (0, false) immediately if the buffer is empty. Safe for concurrent use by
// multiple consumers.
//
// Returns the number of bytes copied and true on success. If buf is smaller than
// the stored entry, the data is truncated to len(buf).
func (rb *RingBuffer) Pop(buf []byte) (int, bool) {
	for {
		pos := rb.tail.Load()
		s := &rb.slots[pos&rb.mask]
		seq := s.sequence.Load()
		diff := int64(seq) - int64(pos+1)

		switch {
		case diff == 0:
			// Slot is published — try to claim it.
			if rb.tail.CompareAndSwap(pos, pos+1) {
				n := int(s.dataLen)
				if n > len(buf) {
					n = len(buf)
				}
				copy(buf[:n], s.data[:n])
				// Reclamation barrier: sets the sequence ahead by one full cycle
				// so the producer will see it as free on the next wrap-around.
				s.sequence.Store(pos + rb.mask + 1)
				return n, true
			}
		case diff < 0:
			// The slot hasn't been published yet. The buffer is empty.
			return 0, false
		}
		// diff > 0: another consumer already claimed this position; retry.
		runtime.Gosched()
	}
}

// PopBatch dequeues up to len(bufs) consecutive entries in a single atomic
// tail advancement. Each bufs[i] must be a pre-allocated slice with sufficient
// capacity; it will be resliced to the actual data length on return.
//
// Returns the number of entries successfully dequeued. Returns 0 if the buffer
// is empty or if another consumer wins the CAS race (caller should retry).
func (rb *RingBuffer) PopBatch(bufs [][]byte) int {
	pos := rb.tail.Load()

	// Phase 1: scan forward from tail to count consecutive published slots.
	var available uint64
	max := uint64(len(bufs))
	for available < max {
		s := &rb.slots[(pos+available)&rb.mask]
		seq := s.sequence.Load()
		diff := int64(seq) - int64(pos+available+1)
		if diff != 0 {
			break
		}
		available++
	}

	if available == 0 {
		return 0
	}

	// Phase 2: claim the entire range with one CAS.
	if !rb.tail.CompareAndSwap(pos, pos+available) {
		return 0
	}

	// Phase 3: copy data out and reclaim each slot.
	for i := uint64(0); i < available; i++ {
		s := &rb.slots[(pos+i)&rb.mask]
		n := int(s.dataLen)
		if n > len(bufs[i]) {
			n = len(bufs[i])
		}
		copy(bufs[i][:n], s.data[:n])
		bufs[i] = bufs[i][:n]
		s.sequence.Store(pos + i + rb.mask + 1)
	}

	return int(available)
}

// Len returns an approximate count of entries currently in the buffer.
// The value is inherently racy under concurrent access but useful for diagnostics.
func (rb *RingBuffer) Len() uint64 {
	return rb.head.Load() - rb.tail.Load()
}

// Cap returns the total slot capacity of the buffer (always a power of two).
func (rb *RingBuffer) Cap() uint64 {
	return rb.size
}

// SlotSize returns the maximum byte capacity of each slot.
func (rb *RingBuffer) SlotSize() uint64 {
	return rb.slotSize
}

func nextPowerOf2(v uint64) uint64 {
	if v == 0 {
		return 1
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v |= v >> 32
	v++
	return v
}
