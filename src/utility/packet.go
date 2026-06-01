//go:build linux

package utility

import "sync"

type Packet struct {
    Buf []byte
    N   int
}

var PacketPool = sync.Pool{
    New: func() any {
        return &Packet{Buf: make([]byte, 1500)}
    },
}
