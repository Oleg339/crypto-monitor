CREATE TABLE IF NOT EXISTS signals (
    id           BIGSERIAL PRIMARY KEY,
    time         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    symbol       TEXT NOT NULL,
    strategy     TEXT NOT NULL,
    direction    TEXT NOT NULL,
    entry        DECIMAL(20,8) NOT NULL,
    stop_loss    DECIMAL(20,8) NOT NULL,
    take_profits DECIMAL(20,8)[] NOT NULL,
    rr_ratio     DECIMAL(10,4) NOT NULL,
    timeframe    TEXT NOT NULL,
    sent         BOOLEAN NOT NULL DEFAULT FALSE,
    sent_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_signals_symbol_time ON signals (symbol, time DESC);
CREATE INDEX IF NOT EXISTS idx_signals_sent ON signals (sent) WHERE NOT sent;
