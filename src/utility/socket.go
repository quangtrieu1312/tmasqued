//go:build linux

package utility

import (
    "errors"
    "fmt"
    "unsafe"
	"syscall"
	"runtime"
	"sync/atomic"

    "golang.org/x/net/ipv4"
    "golang.org/x/net/ipv6"
    "golang.org/x/sys/unix"
)

const MaxSocketBatchSize = 1024

var totalSocketFlushes atomic.Int64
var totalSocketPackets atomic.Int64

type mmsghdr struct {
	Hdr unix.Msghdr
	Len uint32
	_   [4]byte
}

type SocketBatch struct {
    fd     int
    msgs   []mmsghdr
    iovs   []unix.Iovec
    addrs4 []unix.RawSockaddrInet4
    addrs6 []unix.RawSockaddrInet6
    bufs   [MaxSocketBatchSize][1500]byte
    count  int
}


func NewSocketBatch() (*SocketBatch, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_RAW)
    if err != nil {
        return nil, fmt.Errorf("raw socket: %w", err)
    }
    if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_HDRINCL, 1); err != nil {
        unix.Close(fd)
        return nil, fmt.Errorf("IP_HDRINCL: %w", err)
    }
    return &SocketBatch{
        fd:     fd,
        msgs:   make([]mmsghdr, MaxSocketBatchSize),
        iovs:   make([]unix.Iovec, MaxSocketBatchSize),
        addrs4: make([]unix.RawSockaddrInet4, MaxSocketBatchSize),
        addrs6: make([]unix.RawSockaddrInet6, MaxSocketBatchSize),
    }, nil
}

func (b *SocketBatch) Add(pkt []byte) error {
	i := b.count
    n := copy(b.bufs[i][:], pkt)
    switch v := IPVersion(pkt); v {
    case 4:
        if len(pkt) < ipv4.HeaderLen {
            return errors.New("IPv4 packet too short")
        }
        i := b.count
        b.addrs4[i] = unix.RawSockaddrInet4{
            Family: unix.AF_INET,
            Addr:   ([4]byte)(pkt[16:20]),
        }
        b.iovs[i] = unix.Iovec{Base: &b.bufs[i][0]}
        b.iovs[i].SetLen(n)
        b.msgs[i].Hdr.Name = (*byte)(unsafe.Pointer(&b.addrs4[i]))
        b.msgs[i].Hdr.Namelen = unix.SizeofSockaddrInet4
        b.msgs[i].Hdr.Iov = &b.iovs[i]
        b.msgs[i].Hdr.SetIovlen(1)
        b.count++
        return nil
    case 6:
        if len(pkt) < ipv6.HeaderLen {
            return errors.New("IPv6 packet too short")
        }
        i := b.count
        b.addrs6[i] = unix.RawSockaddrInet6{
            Family: unix.AF_INET6,
            Addr:   ([16]byte)(pkt[24:40]),
        }
        b.iovs[i] = unix.Iovec{Base: &pkt[0]}
        b.iovs[i].SetLen(len(pkt))
        b.msgs[i].Hdr.Name = (*byte)(unsafe.Pointer(&b.addrs6[i]))
        b.msgs[i].Hdr.Namelen = unix.SizeofSockaddrInet6
        b.msgs[i].Hdr.Iov = &b.iovs[i]
        b.msgs[i].Hdr.SetIovlen(1)
        b.count++
        return nil
    default:
        return fmt.Errorf("unknown IP version: %d", v)
    }
}

func (b *SocketBatch) Flush(enableStats bool) error {
	if b.count == 0 {
        return nil
    }
	var p runtime.Pinner
    for i := 0; i < b.count; i++ {
        p.Pin(&b.addrs4[i])
        p.Pin(&b.iovs[i])
        p.Pin(&b.bufs[i][0])
    }
    defer p.Unpin()
    _, _, errno := syscall.Syscall6(
        unix.SYS_SENDMMSG,
        uintptr(b.fd),
        uintptr(unsafe.Pointer(&b.msgs[0])),
        uintptr(b.count),
        uintptr(unix.MSG_DONTWAIT),
        0, 0,
    )
	if (enableStats) {
		totalSocketFlushes.Add(1)
		totalSocketPackets.Add(int64(b.count))
	}
    b.count = 0
    if errno != 0 {
        return errno
    }
    return nil

}

func (b *SocketBatch) Full() bool {
    return b.count >= MaxSocketBatchSize
}

func (b *SocketBatch) Empty() bool {
    return b.count == 0
}

// SendOnSocket kept for backward compat / single packet fallback
func SendOnSocket(pkt []byte, enableStats bool) error {
    b, err := NewSocketBatch()
	if err != nil {
		return err
	}
	defer b.Close()
    if err = b.Add(pkt); err != nil {
        return err
    }
    return b.Flush(enableStats)
}

func SocketBatchStats() (flushes, packets int64, avg float64) {
    f := totalSocketFlushes.Load()
    p := totalSocketPackets.Load()
    if f == 0 {
        return f, p, 0
    }
    return f, p, float64(p) / float64(f)
}

func (b *SocketBatch) Close() {
    unix.Close(b.fd)
}
