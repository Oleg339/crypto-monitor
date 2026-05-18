FROM golang:1.23-alpine AS builder

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
    -ldflags="-s -w" -o /out/signal_backtest ./cmd/signal_backtest

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/tradebot /usr/local/bin/tradebot
COPY --from=builder /out/tgauth   /usr/local/bin/tgauth
ENTRYPOINT ["/usr/local/bin/tradebot"]
