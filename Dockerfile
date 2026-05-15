FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git gcc musl-dev curl unzip

ARG TARGETARCH
ARG ZENOH_C_VERSION=1.9.0

RUN case ${TARGETARCH} in \
        "amd64") ZENOH_ARCH=x86_64 ;; \
        "arm64") ZENOH_ARCH=aarch64 ;; \
        *) echo "Unsupported architecture: ${TARGETARCH}" && exit 1 ;; \
    esac && \
    curl -sSL -o /tmp/zc.zip \
        https://github.com/eclipse-zenoh/zenoh-c/releases/download/${ZENOH_C_VERSION}/zenoh-c-${ZENOH_C_VERSION}-${ZENOH_ARCH}-unknown-linux-musl-standalone.zip && \
    unzip -q /tmp/zc.zip -d /opt/zenoh-c && \
    rm /tmp/zc.zip

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux \
    CGO_CFLAGS="-I/opt/zenoh-c/include" \
    CGO_LDFLAGS="-L/opt/zenoh-c/lib -lzenohc" \
    go build -a -o main ./cmd/main

FROM alpine:latest

RUN apk --no-cache add ca-certificates libgcc

WORKDIR /root/

COPY --from=builder /app/main .
COPY --from=builder /opt/zenoh-c/lib/libzenohc.so /usr/lib/

CMD ["./main"]
