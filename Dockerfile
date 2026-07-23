ARG GO_VERSION=1.26.5
ARG XX_VERSION=1.9.0

# Cross-compilation helpers (https://github.com/tonistiigi/xx)
FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx

# Build stage
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

# Copy the build utilities
COPY --from=xx / /

WORKDIR /workspace

# Cache modules first
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy source
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETPLATFORM
ARG TARGETARCH

# Build the webhook for the target platform (static, no CGO)
ENV CGO_ENABLED=0
RUN xx-go build -trimpath -ldflags="-w -s" -o webhook ./cmd/webhook

# Runtime stage — distroless static, non-root
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/webhook /webhook

USER 65532:65532

ENTRYPOINT ["/webhook"]
