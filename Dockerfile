FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git gcc g++ musl-dev cmake make linux-headers pkgconfig

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux make build

FROM alpine:latest

RUN apk --no-cache add ca-certificates libgcc libstdc++ ffmpeg

WORKDIR /root/

COPY --from=builder /app/bin/om1-telemetry ./main
COPY --from=builder /app/.cyclonedds/install/lib/libddsc.so* /usr/lib/

CMD ["./main"]
