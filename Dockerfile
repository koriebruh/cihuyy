# ─── Stage 1: Builder ────────────────────────────────────────────────────────
FROM golang:1.21-alpine AS builder

# git is required by `go mod download` for VCS-based modules.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

# Download dependencies first (cached as a separate layer).
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a statically-linked binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s -extldflags '-static'" \
    -o /matching-engine \
    ./cmd/server

# ─── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM alpine:3.19

# ca-certificates for outbound TLS, tzdata for correct time zones.
RUN apk add --no-cache ca-certificates tzdata wget && \
    # Create a non-privileged user/group that the process will run as.
    addgroup -S nonroot && \
    adduser  -S nonroot -G nonroot

# Copy only the compiled binary from the builder stage.
COPY --from=builder /matching-engine /matching-engine

EXPOSE 8080 9090

USER nonroot

ENTRYPOINT ["/matching-engine"]
