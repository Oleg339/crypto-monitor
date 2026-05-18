# Stage 1: Build all binaries
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

# Download dependencies first for layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree.
COPY . .

# Build all four binaries.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /out/collector ./cmd/collector && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /out/analyzer ./cmd/analyzer && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /out/alerter ./cmd/alerter && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /out/backtester ./cmd/backtester

# Stage 2: collector runtime image
FROM alpine:3.19 AS collector
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/collector /usr/local/bin/collector
ENTRYPOINT ["/usr/local/bin/collector"]

# Stage 3: analyzer runtime image
FROM alpine:3.19 AS analyzer
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/analyzer /usr/local/bin/analyzer
ENTRYPOINT ["/usr/local/bin/analyzer"]

# Stage 4: alerter runtime image
FROM alpine:3.19 AS alerter
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/alerter /usr/local/bin/alerter
ENTRYPOINT ["/usr/local/bin/alerter"]

# Stage 5: backtester runtime image
FROM alpine:3.19 AS backtester
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/backtester /usr/local/bin/backtester
ENTRYPOINT ["/usr/local/bin/backtester"]
