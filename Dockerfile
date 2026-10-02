# Base images are pinned by digest so builds are reproducible and the SLSA
# provenance attests inputs that cannot drift; the tag comments keep them
# human-readable and give Dependabot's docker ecosystem something to bump.

# Cross-compilation helpers (https://github.com/tonistiigi/xx)
# tonistiigi/xx:1.9.0
FROM --platform=$BUILDPLATFORM tonistiigi/xx:1.9.0@sha256:c64defb9ed5a91eacb37f96ccc3d4cd72521c4bd18d5442905b95e2226b0e707 AS xx

# Build stage
# golang:1.26.6-alpine
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder

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

# Runtime stage — distroless static, non-root. The `nonroot` tag carries no
# version at all: without the digest every build silently pulls whatever
# Google pushed last.
# gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

WORKDIR /

COPY --from=builder /workspace/webhook /webhook

USER 65532:65532

ENTRYPOINT ["/webhook"]
