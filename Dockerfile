# ---- Build stage -----------------------------------------------------------
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Cache dependencies first for faster incremental builds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a static, stripped binary. CGO is disabled so the result runs on a
# scratch/distroless base with no libc dependency.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/3xui-exporter .

# ---- Runtime stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/3xui-exporter /usr/bin/3xui-exporter

# Default exporter port (override with LISTEN_ADDR).
EXPOSE 9808

USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/3xui-exporter"]
