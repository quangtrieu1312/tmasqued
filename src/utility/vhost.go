//go:build linux

package utility

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Raw vhost-net + TUN writer. Goal: submit batches of inner packets to a TUN via the
// in-kernel vhost-net worker so they land in tun_napi_receive -> napi_gro_receive in
// 64-frame TUN_MSG_PTR batches (large GRO super-skbs, exactly kernel-WireGuard's path),
// instead of one write()/packet (more=false, no real GRO). See project memory #9/#vhost.
//
// Requires kernel >= 5.18 and a TUN opened IFF_NAPI|IFF_VNET_HDR. We use a SINGLE TX
// virtqueue (index 1, guest->host) so a flow stays in order. Identity-mapped, mlock'd
// arena holds the split vring + packet buffers (desc.addr == userspace pointer, so no
// translation math). Reclaim by polling used.idx (no CALL eventfd read on the hot path).
//
// ABI per include/uapi/linux/vhost{,_types}.h + virtio_ring.h; sequence per rust-vmm/vhost.

const (
	vhostVirtio = 0xAF

	vhostNetVQTx = 1 // guest->host transmit queue (RX=0)

	// virtio feature bits
	vhostNetFVirtioNetHdr = 27
	virtioFVersion1       = 32

	// virtio_net_hdr_v1 size (VERSION_1)
	vnetHdrV1Len = 12

	// vring_desc.flags
	vringDescFNext  = 1
	vringDescFWrite = 2
)

// ioctl number encoding (asm-generic): dir<<30 | size<<16 | type<<8 | nr
func iocW(nr, size uintptr) uintptr { return (1 << 30) | (size << 16) | (vhostVirtio << 8) | nr }
func iocR(nr, size uintptr) uintptr { return (2 << 30) | (size << 16) | (vhostVirtio << 8) | nr }
func iocNone(nr uintptr) uintptr    { return (vhostVirtio << 8) | nr }

// --- vhost uapi structs ---

type vhostVringState struct{ index, num uint32 }
type vhostVringFile struct {
	index uint32
	fd    int32
}
type vhostVringAddr struct {
	index, flags                          uint32
	descUserAddr, usedUserAddr, availAddr uint64
	logGuestAddr                          uint64
}
type vhostMemoryRegion struct {
	guestPhysAddr, memorySize, userspaceAddr, flagsPadding uint64
}

// vhost_memory is variable length (header + regions[]); we hand-build the buffer.

// --- split virtqueue overlays (VERSION_1 => little-endian) ---

type vringDesc struct {
	addr        uint64
	len         uint32
	flags, next uint16
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

type vhostNetWriter struct {
	vhostFd int
	tunFd   int
	kickFd  int
	callFd  int

	n        uint32 // ring entries (power of two)
	bufSize  int
	arena    []byte // mlock'd, identity-mapped
	arenaPtr uintptr

	// vring region pointers (into arena)
	desc     []vringDesc
	availPtr unsafe.Pointer // &[flags u16, idx u16, ring[n] u16, used_event u16]
	usedPtr  unsafe.Pointer // &[flags u16, idx u16, ring[n]{id,len u32}, avail_event u16]
	bufBase  uintptr        // offset of buffer area within arena
	bufOff   int            // byte offset of buffers from arena start

	availIdx uint16 // our running avail index (mod 2^16)
	lastUsed uint16 // our running used index for reclaim
	freeHead uint32 // next descriptor to (re)use, round-robin
}

func (w *vhostNetWriter) availIdxPtr() *uint16 {
	return (*uint16)(unsafe.Add(w.availPtr, 2))
}
func (w *vhostNetWriter) availRing() []uint16 {
	return unsafe.Slice((*uint16)(unsafe.Add(w.availPtr, 4)), w.n)
}
func (w *vhostNetWriter) usedIdxPtr() *uint16 {
	return (*uint16)(unsafe.Add(w.usedPtr, 2))
}

// newVhostNetWriter sets up vhost-net bound to tunFd. tunFd must already be opened
// IFF_TUN|IFF_NO_PI|IFF_VNET_HDR|IFF_NAPI with vnet hdr size 12 and brought up.
func newVhostNetWriter(tunFd, entries, bufSize int) (*vhostNetWriter, error) {
	if entries&(entries-1) != 0 || entries <= 0 {
		return nil, fmt.Errorf("entries must be power of two")
	}
	vfd, err := unix.Open("/dev/vhost-net", unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/vhost-net: %w", err)
	}
	w := &vhostNetWriter{vhostFd: vfd, tunFd: tunFd, n: uint32(entries), bufSize: bufSize}

	// eventfds for kick (we->kernel) and call (kernel->we; we poll used.idx instead but
	// must pass a valid fd).
	kfd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		unix.Close(vfd)
		return nil, fmt.Errorf("eventfd kick: %w", err)
	}
	cfd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		unix.Close(vfd)
		unix.Close(kfd)
		return nil, fmt.Errorf("eventfd call: %w", err)
	}
	w.kickFd, w.callFd = kfd, cfd

	if err := w.layoutArena(); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.setup(); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

// layoutArena allocates one anonymous, locked, populated mapping holding the split
// vring (desc/avail/used) followed by the packet buffers, and wires the overlays.
func (w *vhostNetWriter) layoutArena() error {
	n := int(w.n)
	descBytes := 16 * n
	availBytes := 4 + 2*n + 2 // flags+idx + ring[n] + used_event
	availOff := descBytes
	usedOff := align(availOff+availBytes, 4)
	usedBytes := 4 + 8*n + 2 // flags+idx + ring[n]{id,len} + avail_event
	w.bufOff = align(usedOff+usedBytes, 16)
	total := w.bufOff + n*w.bufSize

	arena, err := unix.Mmap(-1, 0, total, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_POPULATE|unix.MAP_LOCKED)
	if err != nil {
		return fmt.Errorf("mmap arena: %w", err)
	}
	w.arena = arena
	w.arenaPtr = uintptr(unsafe.Pointer(&arena[0]))

	w.desc = unsafe.Slice((*vringDesc)(unsafe.Pointer(&arena[0])), n)
	w.availPtr = unsafe.Pointer(&arena[availOff])
	w.usedPtr = unsafe.Pointer(&arena[usedOff])
	w.bufBase = w.arenaPtr + uintptr(w.bufOff)
	return nil
}

func align(x, a int) int { return (x + a - 1) &^ (a - 1) }

// bufPtr returns the userspace address of buffer slot i.
func (w *vhostNetWriter) bufPtr(i int) uintptr { return w.bufBase + uintptr(i*w.bufSize) }
func (w *vhostNetWriter) bufSlice(i int) []byte {
	return w.arena[w.bufOff+i*w.bufSize : w.bufOff+(i+1)*w.bufSize]
}

func (w *vhostNetWriter) setup() error {
	// 1. SET_OWNER
	if err := ioctl(w.vhostFd, iocNone(0x01), nil); err != nil {
		return fmt.Errorf("SET_OWNER: %w", err)
	}
	// 2. features: negotiate VERSION_1 | VIRTIO_NET_HDR with what the kernel offers.
	var have uint64
	if err := ioctl(w.vhostFd, iocR(0x00, 8), unsafe.Pointer(&have)); err != nil {
		return fmt.Errorf("GET_FEATURES: %w", err)
	}
	want := uint64(1)<<virtioFVersion1 | uint64(1)<<vhostNetFVirtioNetHdr
	feat := have & want
	fmt.Printf("[vhost] GET_FEATURES have=%#x want=%#x feat=%#x (VNET_HDR bit27 offered=%v)\n",
		have, want, feat, have&(1<<vhostNetFVirtioNetHdr) != 0)
	if err := ioctl(w.vhostFd, iocW(0x00, 8), unsafe.Pointer(&feat)); err != nil {
		return fmt.Errorf("SET_FEATURES(%#x): %w", feat, err)
	}
	// 3. SET_MEM_TABLE: one identity-mapped region covering the whole arena.
	memBuf := make([]byte, 8+32) // {nregions u32, pad u32} + 1 region(32B)
	*(*uint32)(unsafe.Pointer(&memBuf[0])) = 1
	reg := (*vhostMemoryRegion)(unsafe.Pointer(&memBuf[8]))
	reg.guestPhysAddr = uint64(w.arenaPtr) // identity: gpa == userspace addr
	reg.memorySize = uint64(len(w.arena))
	reg.userspaceAddr = uint64(w.arenaPtr)
	if err := ioctl(w.vhostFd, iocW(0x03, 8), unsafe.Pointer(&memBuf[0])); err != nil {
		return fmt.Errorf("SET_MEM_TABLE: %w", err)
	}
	// 4. TX vring (index 1)
	st := vhostVringState{index: vhostNetVQTx, num: w.n}
	if err := ioctl(w.vhostFd, iocW(0x10, 8), unsafe.Pointer(&st)); err != nil {
		return fmt.Errorf("SET_VRING_NUM: %w", err)
	}
	base := vhostVringState{index: vhostNetVQTx, num: 0}
	if err := ioctl(w.vhostFd, iocW(0x12, 8), unsafe.Pointer(&base)); err != nil {
		return fmt.Errorf("SET_VRING_BASE: %w", err)
	}
	addr := vhostVringAddr{
		index:        vhostNetVQTx,
		descUserAddr: uint64(uintptr(unsafe.Pointer(&w.desc[0]))),
		availAddr:    uint64(uintptr(w.availPtr)),
		usedUserAddr: uint64(uintptr(w.usedPtr)),
	}
	if err := ioctl(w.vhostFd, iocW(0x11, 40), unsafe.Pointer(&addr)); err != nil {
		return fmt.Errorf("SET_VRING_ADDR: %w", err)
	}
	kick := vhostVringFile{index: vhostNetVQTx, fd: int32(w.kickFd)}
	if err := ioctl(w.vhostFd, iocW(0x20, 8), unsafe.Pointer(&kick)); err != nil {
		return fmt.Errorf("SET_VRING_KICK: %w", err)
	}
	call := vhostVringFile{index: vhostNetVQTx, fd: int32(w.callFd)}
	if err := ioctl(w.vhostFd, iocW(0x21, 8), unsafe.Pointer(&call)); err != nil {
		return fmt.Errorf("SET_VRING_CALL: %w", err)
	}
	// 5. attach the tun backend LAST -> activates the datapath.
	be := vhostVringFile{index: vhostNetVQTx, fd: int32(w.tunFd)}
	if err := ioctl(w.vhostFd, iocW(0x30, 8), unsafe.Pointer(&be)); err != nil {
		return fmt.Errorf("NET_SET_BACKEND: %w", err)
	}
	return nil
}

// vhostFence is a process-wide full memory barrier (atomic RMW); combined with x86-TSO
// store ordering and the kick syscall (itself a full barrier), it keeps the descriptor
// and ring-slot writes ordered before the avail.idx publish the vhost worker reads.
var vhostFence uint32

func fence() { atomic.AddUint32(&vhostFence, 1) }

// Submit copies pkt (prefixed with a zeroed 12-byte virtio_net_hdr) into a free buffer
// slot, fills its descriptor, and posts it into the avail ring (bumping the LOCAL
// avail index — Flush publishes it). Returns false if the ring is full (caller should
// Flush to kick the worker, which drains + advances used.idx, then retry).
var vhostDbgN int

func (w *vhostNetWriter) Submit(pkt []byte) bool {
	w.Reclaim()
	if w.availIdx-w.lastUsed >= uint16(w.n) {
		if vhostDbgN < 8 {
			vhostDbgN++
			fmt.Printf("[vhost] FULL availIdx=%d lastUsed=%d usedIdx=%d freeHead=%d n=%d\n",
				w.availIdx, w.lastUsed, *w.usedIdxPtr(), w.freeHead, w.n)
		}
		return false // ring full
	}
	slot := int(w.freeHead & (w.n - 1))
	buf := w.bufSlice(slot)
	if vnetHdrV1Len+len(pkt) > len(buf) {
		return true // too big — drop silently (counted by caller)
	}
	for i := 0; i < vnetHdrV1Len; i++ {
		buf[i] = 0 // GSO_NONE, no flags
	}
	copy(buf[vnetHdrV1Len:], pkt)
	d := &w.desc[slot]
	d.addr = uint64(w.bufPtr(slot))
	d.len = uint32(vnetHdrV1Len + len(pkt))
	d.flags = 0 // single buffer, device-read-only
	d.next = 0
	w.availRing()[w.availIdx&uint16(w.n-1)] = uint16(slot)
	w.availIdx++
	w.freeHead++
	return true
}

// Flush publishes the accumulated avail index to the worker and kicks it.
func (w *vhostNetWriter) Flush() {
	fence() // descriptor + ring writes before the idx publish
	*w.availIdxPtr() = w.availIdx
	fence()
	var one uint64 = 1
	unix.Write(w.kickFd, (*[8]byte)(unsafe.Pointer(&one))[:])
}

// Reclaim advances lastUsed over completed entries (their buffer slots become reusable).
func (w *vhostNetWriter) Reclaim() {
	fence()
	w.lastUsed = *w.usedIdxPtr()
}

func (w *vhostNetWriter) Close() {
	if w.tunFd >= 0 {
		be := vhostVringFile{index: vhostNetVQTx, fd: -1}
		ioctl(w.vhostFd, iocW(0x30, 8), unsafe.Pointer(&be))
	}
	if w.arena != nil {
		unix.Munmap(w.arena)
	}
	if w.kickFd > 0 {
		unix.Close(w.kickFd)
	}
	if w.callFd > 0 {
		unix.Close(w.callFd)
	}
	unix.Close(w.vhostFd)
}
