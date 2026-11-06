# Base images are pinned by digest so builds are reproducible and the SLSA
# provenance attests inputs that cannot drift; the tag comments keep them
# human-readable and give Dependabot's docker ecosystem something to bump.

# Cross-compilation helpers (https://github.com/tonistiigi/xx)
# tonistiigi/xx:1.10.0
FROM --platform=$BUILDPLATFORM tonistiigi/xx:1.10.0@sha256:40fe21f44bb8cddf1eb1226db190d0a984118f62ee02a91b77d99448d28264a6 AS xx

# Build stage
# golang:1.27.3-alpine
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS builder

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
FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

WORKDIR /

COPY --from=builder /workspace/webhook /webhook

USER 65532:65532

ENTRYPOINT ["/webhook"]
