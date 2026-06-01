FROM ubuntu:24.04 AS builder

WORKDIR /server
RUN DEBIAN_FRONTEND=noninteractive apt-get update && apt upgrade -y && \
        apt-get install -y \
    golang clang llvm libelf-dev libbpf-dev linux-headers-generic
RUN ln -sf /usr/include/$(uname -m)-linux-gnu/asm /usr/include/asm
COPY src /tmasqued/src
COPY lib /tmasqued/lib
COPY build.sh /tmasqued/build.sh
RUN chmod +x /tmasqued/build.sh && /tmasqued/build.sh

FROM ubuntu:24.04 AS runner

RUN DEBIAN_FRONTEND=noninteractive apt-get update && apt upgrade -y && \
        apt-get install -y \
        iproute2 iptables ethtool openssl sqlite3 curl jq zip

COPY ./*.conf /etc/tmasqued/
COPY extras /etc/tmasqued/extras
COPY scripts /etc/tmasqued/scripts
COPY --from=builder /tmasqued/build/bin /usr/sbin/tmasqued

RUN ln -s /etc/tmasqued/scripts/run.sh /run.sh

RUN chmod +x /run.sh
