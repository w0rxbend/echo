FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /matrix-proxy ./cmd/matrix-proxy

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /matrix-proxy .
USER app
EXPOSE 8080
ENTRYPOINT ["/app/matrix-proxy"]
CMD ["-config", "configs/config.yaml"]
