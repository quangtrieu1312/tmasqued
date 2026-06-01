#pragma once

#include <linux/types.h>
#include <bpf/bpf_helpers.h>

/* Mirrors NatRevKey in nat.go — do not reorder fields. */
struct nat_rev_key {
    __u8  wan_ip[4];
    __u8  dst_ip[4];   /* remote server IP (src IP of the reply) */
    __u16 wan_port;    /* our ephemeral WAN port, network byte order */
    __u16 dst_port;    /* remote server port,    network byte order */
    __u8  proto;
    __u8  pad[3];
};

/* Mirrors NatRevVal in nat.go — packed, 16 bytes. */
struct nat_rev_val {
    __u8  client_ip[4];
    __u16 client_port;
    __u16 pad;
    __u64 last_seen_ns;
};

struct {
    __uint(type,        BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 65536);
    __type(key,         struct nat_rev_key);
    __type(value,       struct nat_rev_val);
    __uint(pinning,     LIBBPF_PIN_BY_NAME); /* pins to /sys/fs/bpf/nat_rev_table */
} nat_rev_table SEC(".maps");
