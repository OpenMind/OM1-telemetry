FROM golang:1.25-alpine AS builder

# git/cmake/make/gcc build CycloneDDS from source (via `make build`'s
# install-cyclonedds step, below) — Alpine/musl isn't CycloneDDS's primary
# tested platform; if this build breaks, it's the first place to look.
RUN apk add --no-cache git gcc g++ musl-dev cmake make linux-headers

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

# Builds CycloneDDS from source into .cyclonedds/install (see Makefile), then
# generates the idlc type support and builds the binary — same recipe every
# developer and CI use, no distro package divergence.
RUN CGO_ENABLED=1 GOOS=linux make build

FROM alpine:latest

RUN apk --no-cache add ca-certificates libgcc libstdc++ ffmpeg

WORKDIR /root/

COPY --from=builder /app/bin/om1-telemetry ./main
COPY --from=builder /app/.cyclonedds/install/lib/libddsc.so* /usr/lib/

CMD ["./main"]
