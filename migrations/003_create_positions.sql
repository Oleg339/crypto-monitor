CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE;

CREATE TABLE IF NOT EXISTS indicators (
    time         TIMESTAMPTZ NOT NULL,
    symbol       TEXT NOT NULL,
    interval     TEXT NOT NULL,
    ma10         DECIMAL(20,8),
    ma20         DECIMAL(20,8),
    ma60         DECIMAL(20,8),
    rsi14        DECIMAL(10,4),
    atr14        DECIMAL(20,8),
    volume_ratio DECIMAL(10,4),
    PRIMARY KEY (time, symbol, interval)
);

SELECT create_hypertable('indicators', 'time', if_not_exists => TRUE);

CREATE TABLE IF NOT EXISTS positions (
    id           BIGSERIAL PRIMARY KEY,
    signal_id    BIGINT REFERENCES signals(id),
    symbol       TEXT NOT NULL,
    direction    TEXT NOT NULL,
    entry        DECIMAL(20,8) NOT NULL,
    stop_loss    DECIMAL(20,8) NOT NULL,
    take_profits DECIMAL(20,8)[],
    opened_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at    TIMESTAMPTZ,
    close_price  DECIMAL(20,8),
    pnl_pct      DECIMAL(10,4),
    status       TEXT NOT NULL DEFAULT 'open'
);
