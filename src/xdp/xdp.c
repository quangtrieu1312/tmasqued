//go:build ignore

#include <linux/bpf.h>
#include <linux/in.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/udp.h>
#include <linux/tcp.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_endian.h>
#include "nat.h"

struct {
    __uint(type, BPF_MAP_TYPE_XSKMAP);
    __uint(max_entries, 64);
    __type(key, __u32);
    __type(value, __u32);
} xsks_quic SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_XSKMAP);
    __uint(max_entries, 64);
    __type(key, __u32);
    __type(value, __u32);
} xsks_fwd SEC(".maps");


/*
 * Incremental checksum update (RFC 1624) when a 32-bit field changes.
 * Uses bpf_csum_diff so byte-order is handled correctly on LE hosts.
 */
static __always_inline void csum_replace4(__u16 *csum,
                                          __be32 old,
                                          __be32 new) {
    __u32 tmp = ~((__u32)*csum) & 0xffff;
    tmp += ~old & 0xffff;
    tmp += ~(old >> 16) & 0xffff;
    tmp += new & 0xffff;
    tmp += (new >> 16) & 0xffff;
    tmp = (tmp & 0xffff) + (tmp >> 16);
    tmp = (tmp & 0xffff) + (tmp >> 16);
    *csum = ~tmp;
}

/* Incremental update when a 16-bit field changes (zero-pads to 4 B). */
static __always_inline void csum_replace2(__u16 *sum,
                                          __u16  old,
                                          __u16  new) {
    __u16 csum = ~*sum;
    csum += ~old;
    csum += csum < (__u16)~old;  // carry
    csum += new;
    csum += csum < (__u16)new;   // carry
    *sum = ~csum;
}

/* ── XDP program ─────────────────────────────────────────────────────────── */

SEC("xdp")
int masque_xdp_prog(struct xdp_md *ctx) {
    void *data_end = (void *)(long)ctx->data_end;
    void *data     = (void *)(long)ctx->data;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end) return XDP_PASS;
    if (eth->h_proto != __constant_htons(ETH_P_IP)) return XDP_PASS;

    struct iphdr *ip = (void *)(eth + 1);
    if ((void *)(ip + 1) > data_end) return XDP_PASS;

    __u32 ihl = (ip->ihl & 0x0f) * 4;
    if (ihl < 20) return XDP_PASS;

    if (ip->protocol == IPPROTO_UDP) {
        struct udphdr *udp = (void *)ip + ihl;
        if ((void *)(udp + 1) > data_end) return XDP_PASS;

        /* Redirect QUIC (UDP/443) to the AF_XDP socket for this queue. */
        if (udp->dest == __constant_htons(443)) {
            __u32 queue_id = ctx->rx_queue_index;
            if (bpf_map_lookup_elem(&xsks_quic, &queue_id))
                return bpf_redirect_map(&xsks_quic, queue_id, XDP_PASS);
        }

        /* DNAT for UDP return traffic (replies to SNAT'd client flows). */
        struct nat_rev_key key = {};
        __builtin_memcpy(key.wan_ip, &ip->daddr, 4);
        __builtin_memcpy(key.dst_ip, &ip->saddr, 4);
        key.wan_port = udp->dest;   /* ephemeral WAN port we allocated */
        key.dst_port = udp->source; /* remote server's port             */
        key.proto    = IPPROTO_UDP;

        struct nat_rev_val *val = bpf_map_lookup_elem(&nat_rev_table, &key);
        if (val) {
            __be32 new_daddr;
            __builtin_memcpy(&new_daddr, val->client_ip, 4);
            __u16  new_dport = val->client_port;
            __be32 old_daddr = ip->daddr;
            __u16 old_dport = udp->dest;

            /* Update IP header checksum for daddr change. */
            csum_replace4(&ip->check, old_daddr, new_daddr);

            /* Update UDP checksum if present (0 means disabled). */
            if (udp->check) {
                csum_replace4(&udp->check, old_daddr, new_daddr);
                csum_replace2(&udp->check, old_dport, new_dport);
            }

            ip->daddr  = new_daddr;
            udp->dest  = new_dport;

            val->last_seen_ns = bpf_ktime_get_ns();
            __u32 queue_id = ctx->rx_queue_index;
            if (bpf_map_lookup_elem(&xsks_fwd, &queue_id))
                return bpf_redirect_map(&xsks_fwd, queue_id, XDP_PASS);
        }
    } else if (ip->protocol == IPPROTO_TCP) {
        struct tcphdr *tcp = (void *)ip + ihl;
        if ((void *)(tcp + 1) > data_end) return XDP_PASS;

        /* DNAT for TCP return traffic. */
        struct nat_rev_key key = {};
        __builtin_memcpy(key.wan_ip, &ip->daddr, 4);
        __builtin_memcpy(key.dst_ip, &ip->saddr, 4);
        key.wan_port = tcp->dest;
        key.dst_port = tcp->source;
        key.proto    = IPPROTO_TCP;

        struct nat_rev_val *val = bpf_map_lookup_elem(&nat_rev_table, &key);
        if (val) {
            __be32 new_daddr;
            __builtin_memcpy(&new_daddr, val->client_ip, 4);
            __u16  new_dport = val->client_port;
            __be32 old_daddr = ip->daddr;
            __u16 old_dport = tcp->dest;

            csum_replace4(&ip->check,  old_daddr, new_daddr);
            csum_replace4(&tcp->check, old_daddr, new_daddr);
            csum_replace2(&tcp->check, old_dport, new_dport);

            ip->daddr  = new_daddr;
            tcp->dest  = new_dport;

            val->last_seen_ns = bpf_ktime_get_ns();
            __u32 queue_id = ctx->rx_queue_index;
            if (bpf_map_lookup_elem(&xsks_fwd, &queue_id))
                return bpf_redirect_map(&xsks_fwd, queue_id, XDP_PASS);
        }
    }

    return XDP_PASS;
}

char _license[] SEC("license") = "GPL";
