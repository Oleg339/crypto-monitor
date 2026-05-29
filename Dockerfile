FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" -o /out/tradebot ./cmd/tradebot && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" -o /out/tgauth ./cmd/tgauth && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" -o /out/collector ./cmd/collector && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" -o /out/analyzer ./cmd/analyzer && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" -o /out/alerter ./cmd/alerter

# --- tradebot ---
FROM alpine:3.21 AS tradebot
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/tradebot /usr/local/bin/tradebot
COPY --from=builder /out/tgauth   /usr/local/bin/tgauth
ENTRYPOINT ["/usr/local/bin/tradebot"]

# --- collector ---
FROM alpine:3.21 AS collector
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/collector /usr/local/bin/collector
ENTRYPOINT ["/usr/local/bin/collector"]

# --- analyzer ---
FROM alpine:3.21 AS analyzer
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/analyzer /usr/local/bin/analyzer
ENTRYPOINT ["/usr/local/bin/analyzer"]

# --- alerter ---
FROM alpine:3.21 AS alerter
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/alerter /usr/local/bin/alerter
ENTRYPOINT ["/usr/local/bin/alerter"]
