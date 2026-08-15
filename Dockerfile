FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git gcc g++ musl-dev cmake make linux-headers

# CycloneDDS has no prebuilt static-musl release (unlike the old zenoh-c
# dependency this replaced), so it's built from source here. Alpine/musl is
# not CycloneDDS's primary tested platform — if this build breaks, it's the
# first place to look; consider switching the builder stage to a glibc base
# (e.g. golang:1.25-bookworm + `apt install cyclonedds-dev`) instead.
ARG CYCLONEDDS_VERSION=0.10.5

RUN git clone --branch ${CYCLONEDDS_VERSION} --depth 1 \
        https://github.com/eclipse-cyclonedds/cyclonedds.git /tmp/cyclonedds && \
    mkdir /tmp/cyclonedds/build && cd /tmp/cyclonedds/build && \
    cmake -DCMAKE_INSTALL_PREFIX=/opt/cyclonedds -DBUILD_EXAMPLES=OFF -DBUILD_TESTING=OFF .. && \
    cmake --build . --target install -- -j"$(nproc)" && \
    rm -rf /tmp/cyclonedds

ENV PKG_CONFIG_PATH=/opt/cyclonedds/lib/pkgconfig
ENV PATH="/opt/cyclonedds/bin:${PATH}"
ENV LD_LIBRARY_PATH="/opt/cyclonedds/lib"

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux make idl-gen && \
    CGO_ENABLED=1 GOOS=linux go build -a -o main ./cmd/main

FROM alpine:latest

RUN apk --no-cache add ca-certificates libgcc libstdc++ ffmpeg

WORKDIR /root/

COPY --from=builder /app/main .
COPY --from=builder /opt/cyclonedds/lib/libddsc.so* /usr/lib/

CMD ["./main"]
