# Build from repository root:
# docker buildx build --platform linux/arm64 -f guest/Dockerfile \
#   --output type=local,dest=/tmp/sapphire-guest-assets guest
# The context is guest/. Nothing executes a VM or contacts a host Docker socket.
ARG ALPINE_IMAGE=alpine:3.23.6@sha256:ae959799024a09da597284763c417a6e820e020fad0634c5267cf4f125e682bf
ARG GO_IMAGE=golang:1.26.8-alpine3.23@sha256:9bba48a62950bb149df18491ed0a7ba9a8edf38907e7a5bb53c1972df2b20042

FROM --platform=linux/arm64 ${GO_IMAGE} AS agent
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test ./internal/guest && \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
    -ldflags="-s -w -buildid=" -o /build/sapphire-guest ./cmd/sapphire-guest

FROM --platform=linux/arm64 ${ALPINE_IMAGE} AS toolchain
ARG KERNEL_VERSION=6.18.57
ARG KERNEL_SHA256=5bd9acdc82ec5efc3790ae840e5aaa40dfde7efff261cf6dde902dc570acf98c
ARG ALPINE_ROOTFS_SHA256=b17a57958e29735ff0e6e64254d958e65903687a70ca30cc430b33a965ad49d7
ARG SOURCE_DATE_EPOCH=1792540800
ARG BUILD_JOBS=4
ENV KERNEL_VERSION=${KERNEL_VERSION} ALPINE_ROOTFS_SHA256=${ALPINE_ROOTFS_SHA256} \
    SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} BUILD_JOBS=${BUILD_JOBS}
COPY download-apks.sh build-apks.lock runtime-apks.lock /src/
RUN sh /src/download-apks.sh /src/build-apks.lock /build/build-apks && \
    apk add --no-network /build/build-apks/*.apk && \
    sh /src/download-apks.sh /src/runtime-apks.lock /build/runtime-apks
RUN curl -fL --retry 3 "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" -o /build/linux.tar.xz && \
    printf '%s  /build/linux.tar.xz\n' "$KERNEL_SHA256" | sha256sum -c - && \
    mkdir -p /build/linux && tar -xJf /build/linux.tar.xz -C /build/linux --strip-components=1 && \
    curl -fL --retry 3 https://dl-cdn.alpinelinux.org/alpine/v3.23/releases/aarch64/alpine-minirootfs-3.23.6-aarch64.tar.gz -o /build/alpine-minirootfs.tar.gz && \
    printf '%s  /build/alpine-minirootfs.tar.gz\n' "$ALPINE_ROOTFS_SHA256" | sha256sum -c -
FROM toolchain AS kernel
COPY kernel.config build-kernel.sh /src/
RUN sh /src/build-kernel.sh

FROM toolchain AS assets
ARG INCLUDE_K3S=0
ARG K3S_VERSION=v1.37.1+k3s1
ARG K3S_SHA256=a1561ca4aef8b99f5588a840d4468ac39c8c1cd92470bbcb9634823fa221741a
ENV INCLUDE_K3S=${INCLUDE_K3S} K3S_VERSION=${K3S_VERSION} K3S_SHA256=${K3S_SHA256}
COPY build-assets.sh udhcpc.script /src/
COPY initramfs/ /src/initramfs/
COPY --from=kernel /out/ /out/
COPY --from=kernel /build/modules/ /build/modules/
COPY --from=kernel /build/kernel-release /build/kernel-release
COPY --from=kernel /build/linux/usr/gen_init_cpio /build/gen_init_cpio
COPY --from=agent /build/sapphire-guest /build/sapphire-guest
RUN sh /src/build-assets.sh

FROM scratch AS export
COPY --from=assets /out/ /
