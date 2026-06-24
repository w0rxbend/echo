FROM golang:1.26-alpine3.24 AS builder
# TARGETOS/TARGETARCH/TARGETVARIANT are injected by docker/build-push-action
# in CI for multi-arch builds. For local builds they default to the host arch.
ARG TARGETOS=linux
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /matrix-proxy ./cmd/matrix-proxy

# Distroless contains only ca-certificates and the timezone database —
# no shell, no package manager, no OS utilities. Runs as nonroot (UID 65532).
FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /app
COPY --from=builder /matrix-proxy .
EXPOSE 8080
ENTRYPOINT ["/app/matrix-proxy"]
CMD ["-config", "configs/config.yaml"]
