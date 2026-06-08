//go:build linux

package utility

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Minimal raw io_uring writer: batch N writes to a single fd in one io_uring_enter
// syscall instead of N write() syscalls. Used to drive the IFF_NAPI forward TUN
// (see tun_uring.go) so the per-packet write cost no longer caps single-flow
// throughput, while the kernel GRO/host-TSO ordering of the IFF_NAPI path is kept.
//
// No SQPOLL (would pin a core), no fixed buffers (kept simple): each Submit copies
// the packet into a slot of an owned arena, queues an IORING_OP_WRITE SQE, and Flush
// submits the whole batch and reaps every completion (so all arena slots are free
// again before the next batch). One enter() amortizes the syscall over the batch.

const (
	sysIOUringSetup = 425
	sysIOUringEnter = 426

	ioringOpWrite = 23

	ioringOffSQRing = 0
	ioringOffCQRing = 0x8000000
	ioringOffSQEs   = 0x10000000

	ioringEnterGetevents = 1
	ioringFeatSingleMmap = 1
)

type ioSQRingOffsets struct {
	head, tail, ringMask, ringEntries, flags, dropped, array, resv1 uint32
	userAddr                                                        uint64
}

type ioCQRingOffsets struct {
	head, tail, ringMask, ringEntries, overflow, cqes, flags, resv1 uint32
	userAddr                                                        uint64
}

type ioUringParams struct {
	sqEntries, cqEntries, flags, sqThreadCPU, sqThreadIdle, features, wqFd uint32
	resv                                                                  [3]uint32
	sqOff                                                                 ioSQRingOffsets
	cqOff                                                                 ioCQRingOffsets
}

type ioUringSQE struct {
	opcode                       uint8
	flags                        uint8
	ioprio                       uint16
	fd                           int32
	off                          uint64
	addr                         uint64
	len                          uint32
	opFlags                      uint32
	userData                     uint64
	bufIndex, personality        uint16
	spliceFdIn                   int32
	addr3, pad2                  uint64
}

type ioUringCQE struct {
	userData uint64
	res      int32
	flags    uint32
}

// uringWriter batches writes to one fd via io_uring.
type uringWriter struct {
	fd      int    // io_uring fd
	entries uint32 // ring depth (power of two)

	sqRing []byte
	cqRing []byte
	sqesMM []byte

	// SQ pointers (into sqRing)
	sqHead  *uint32
	sqTail  *uint32
	sqMask  uint32
	sqArray []uint32
	sqes    []ioUringSQE

	// CQ pointers (into cqRing)
	cqHead *uint32
	cqTail *uint32
	cqMask uint32
	cqes   []ioUringCQE

	// owned write buffers: entries slots of maxFrame bytes
	arena    []byte
	maxFrame int
	queued   uint32 // SQEs queued since last Flush
	tail     uint32 // running SQ tail
}

func newUringWriter(fd, entries, maxFrame int) (*uringWriter, error) {
	var p ioUringParams
	r1, _, e := unix.Syscall(sysIOUringSetup, uintptr(entries), uintptr(unsafe.Pointer(&p)), 0)
	if e != 0 {
		return nil, fmt.Errorf("io_uring_setup: %v", e)
	}
	ringFd := int(r1)

	sqSize := p.sqOff.array + p.sqEntries*4
	cqSize := p.cqOff.cqes + p.cqEntries*uint32(unsafe.Sizeof(ioUringCQE{}))
	if p.features&ioringFeatSingleMmap != 0 {
		if cqSize > sqSize {
			sqSize = cqSize
		}
		cqSize = sqSize
	}

	sqRing, err := unix.Mmap(ringFd, ioringOffSQRing, int(sqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		unix.Close(ringFd)
		return nil, fmt.Errorf("mmap sq: %w", err)
	}
	var cqRing []byte
	if p.features&ioringFeatSingleMmap != 0 {
		cqRing = sqRing
	} else {
		cqRing, err = unix.Mmap(ringFd, ioringOffCQRing, int(cqSize),
			unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
		if err != nil {
			unix.Close(ringFd)
			return nil, fmt.Errorf("mmap cq: %w", err)
		}
	}
	sqeSize := int(p.sqEntries) * int(unsafe.Sizeof(ioUringSQE{}))
	sqesMM, err := unix.Mmap(ringFd, ioringOffSQEs, sqeSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		unix.Close(ringFd)
		return nil, fmt.Errorf("mmap sqes: %w", err)
	}

	w := &uringWriter{
		fd: ringFd, entries: p.sqEntries,
		sqRing: sqRing, cqRing: cqRing, sqesMM: sqesMM,
		maxFrame: maxFrame,
		arena:    make([]byte, int(p.sqEntries)*maxFrame),
	}
	base := unsafe.Pointer(&sqRing[0])
	w.sqHead = (*uint32)(unsafe.Add(base, p.sqOff.head))
	w.sqTail = (*uint32)(unsafe.Add(base, p.sqOff.tail))
	w.sqMask = *(*uint32)(unsafe.Add(base, p.sqOff.ringMask))
	w.sqArray = unsafe.Slice((*uint32)(unsafe.Add(base, p.sqOff.array)), p.sqEntries)
	w.sqes = unsafe.Slice((*ioUringSQE)(unsafe.Pointer(&sqesMM[0])), p.sqEntries)

	cbase := unsafe.Pointer(&cqRing[0])
	w.cqHead = (*uint32)(unsafe.Add(cbase, p.cqOff.head))
	w.cqTail = (*uint32)(unsafe.Add(cbase, p.cqOff.tail))
	w.cqMask = *(*uint32)(unsafe.Add(cbase, p.cqOff.ringMask))
	w.cqes = unsafe.Slice((*ioUringCQE)(unsafe.Add(cbase, p.cqOff.cqes)), p.cqEntries)
	w.tail = atomic.LoadUint32(w.sqTail)
	return w, nil
}

// Submit copies pkt into an arena slot and queues a write SQE. Caller must Flush
// when queued reaches the ring depth (Full) or the batch ends.
func (w *uringWriter) Submit(targetFd int, pkt []byte) {
	idx := w.tail & w.sqMask
	slot := w.arena[int(idx)*w.maxFrame:]
	n := copy(slot, pkt)
	sqe := &w.sqes[idx]
	*sqe = ioUringSQE{}
	sqe.opcode = ioringOpWrite
	sqe.fd = int32(targetFd)
	sqe.addr = uint64(uintptr(unsafe.Pointer(&slot[0])))
	sqe.len = uint32(n)
	sqe.userData = uint64(idx)
	w.sqArray[idx] = idx
	w.tail++
	w.queued++
}

func (w *uringWriter) Full() bool { return w.queued >= w.entries }

// Flush publishes the queued SQEs, enters the kernel to submit them all, and reaps
// every completion (freeing the arena). Returns the count of failed writes.
func (w *uringWriter) Flush() (fails int) {
	if w.queued == 0 {
		return 0
	}
	n := w.queued
	atomic.StoreUint32(w.sqTail, w.tail) // publish
	_, _, e := unix.Syscall6(sysIOUringEnter, uintptr(w.fd),
		uintptr(n), uintptr(n), uintptr(ioringEnterGetevents), 0, 0)
	if e != 0 {
		w.queued = 0
		return int(n) // submission failed wholesale
	}
	// reap exactly n completions
	got := uint32(0)
	for got < n {
		head := atomic.LoadUint32(w.cqHead)
		tail := atomic.LoadUint32(w.cqTail)
		if head == tail {
			// not all landed yet; ask kernel to wait for the rest
			unix.Syscall6(sysIOUringEnter, uintptr(w.fd), 0,
				uintptr(n-got), uintptr(ioringEnterGetevents), 0, 0)
			continue
		}
		for ; head != tail; head++ {
			c := &w.cqes[head&w.cqMask]
			if c.res < 0 {
				fails++
			}
			got++
		}
		atomic.StoreUint32(w.cqHead, head)
	}
	w.queued = 0
	return fails
}

func (w *uringWriter) Close() {
	if w.sqesMM != nil {
		unix.Munmap(w.sqesMM)
	}
	if w.cqRing != nil && len(w.cqRing) > 0 && &w.cqRing[0] != &w.sqRing[0] {
		unix.Munmap(w.cqRing)
	}
	if w.sqRing != nil {
		unix.Munmap(w.sqRing)
	}
	unix.Close(w.fd)
}
