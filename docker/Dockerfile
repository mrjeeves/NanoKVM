# Pin the whole build to linux/amd64. The Sophgo host-tools cross-gcc is an
# x86_64-only binary, so every stage (and the final image) must be amd64 — then
# it runs under Rosetta/QEMU on Apple Silicon. All other stages derive from
# base_apt and inherit this platform. On a native amd64 host it is a no-op.
FROM --platform=linux/amd64 ubuntu:24.04 AS base_apt

# RUN echo 'Acquire::http::Proxy "http://cache.lan:3142";' > /etc/apt/apt.conf.d/00cacher
RUN userdel -r ubuntu
RUN apt update \
    && apt install wget git cmake build-essential python3 python3-venv python3-pip autoconf automake libtool gosu -y

ARG DOCKER_USER=build
RUN useradd -m "$DOCKER_USER"
USER $DOCKER_USER

FROM base_apt AS host_tools

USER root
RUN wget -O /tmp/host-tools.tar.gz https://sophon-file.sophon.cn/sophon-prod-s3/drive/23/03/07/16/host-tools.tar.gz \
    && tar -C /usr/local -zxvf /tmp/host-tools.tar.gz  \
    && rm -f /tmp/host-tools.tar.gz \
    && export PATH=$PATH:/usr/local/host-tools/gcc/riscv64-linux-musl-x86_64/bin \
    && echo "Installed host-tools gcc" && riscv64-unknown-linux-musl-gcc -v

FROM base_apt AS golang

USER root
RUN bash -c 'wget -O /tmp/go.tar.gz https://go.dev/dl/go1.25.0.linux-$([ `uname -m` == "x86_64" ] && echo "amd64" || uname -m).tar.gz' \
    && tar -C /usr/local -zxvf /tmp/go.tar.gz \
    && export PATH=$PATH:/usr/local/go/bin \
    && echo "Installed golang" &&  go version

COPY server /tmp/go_cache

ENV PATH="$PATH:/usr/local/go/bin"

RUN cd /tmp/go_cache \
    && echo "Caching go dep" \
    && go mod download \
    && go mod tidy \
    && cd / \
    && rm -rf /tmp/go_cache

FROM base_apt AS sdk

RUN cd ~ \
    && git clone https://github.com/Sipeed/MaixCDK \
    && cd MaixCDK \
    && python3 -m venv . \
    && . ./bin/activate \
    && pip install -U -r requirements.txt 

COPY --chown=$DOCKER_USER . /home/$DOCKER_USER/NanoKVM

RUN cd ~/MaixCDK \
    && . ./bin/activate \
    && cd ~/NanoKVM/support/sg2002 \
    && ./build kvm_system

# ── Lean server builder: Go + host-tools only (NO MaixCDK). ───────────────────
# Building NanoKVM-Server needs only Go, the riscv64 cross toolchain, and the
# checked-in shared libs + headers (server/dl_lib, server/include). The MaixCDK
# SDK is only needed to *regenerate* libkvm (`make support`), which is rare —
# libkvm is committed. Building `--target server` lets BuildKit prune the heavy
# `sdk` (MaixCDK) stage entirely, so the common path stays fast and reliable
# even under amd64 emulation on Apple Silicon. `make app`/`build-risc` use this.
FROM base_apt AS server

# Install golang
COPY --from=golang /usr/local/go /usr/local/go
COPY --chown=$DOCKER_USER --from=golang /root/go /home/$DOCKER_USER/go
ENV PATH="$PATH:/usr/local/go/bin"

# Install host tools
COPY --from=host_tools /usr/local/host-tools /usr/local/host-tools
ENV PATH="$PATH:/usr/local/host-tools/gcc/riscv64-linux-musl-x86_64/bin"

COPY --chmod=+x docker/entrypoint /entrypoint

RUN echo "Verify build tools" \
    && echo "Installed host-tools gcc" && riscv64-unknown-linux-musl-gcc -v \
    && echo "Installed golang" && go version

USER root
RUN chmod 0755 /entrypoint \
 && sed "s/\$DOCKER_USER/$DOCKER_USER/g" -i /entrypoint

ENTRYPOINT ["/entrypoint"]

# ── Full builder: server + MaixCDK SDK, for `make support` (regenerating libkvm).
# Only built on demand (`make full-image`); the everyday server build never
# pulls this in.
FROM server AS full
USER root
COPY --from=sdk /home/$DOCKER_USER/MaixCDK /home/$DOCKER_USER/MaixCDK