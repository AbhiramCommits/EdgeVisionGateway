-- EdgeVisionGateway storage schema.
--
-- detections is declaratively partitioned by captured_at (one partition per
-- UTC day). Every partition carries its own indexes; the helper function
-- evg_ensure_partition() creates both the partition and those indexes
-- idempotently, so the gateway can pre-create the partitions it is about to
-- need without racing on ad-hoc DDL from application code.

CREATE TABLE IF NOT EXISTS devices (
    device_id    text PRIMARY KEY,
    site_id      text NOT NULL DEFAULT '',
    token_hash   text NOT NULL,
    status       text NOT NULL DEFAULT 'active',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS detections (
    id          bigint GENERATED ALWAYS AS IDENTITY,
    device_id   text        NOT NULL,
    frame_id    text        NOT NULL,
    captured_at timestamptz NOT NULL,
    label       text        NOT NULL,
    confidence  real        NOT NULL,
    x           int         NOT NULL,
    y           int         NOT NULL,
    w           int         NOT NULL,
    h           int         NOT NULL,
    PRIMARY KEY (id, captured_at)
) PARTITION BY RANGE (captured_at);

-- Creates detections_YYYYMMDD plus its two query indexes. Idempotent.
CREATE OR REPLACE FUNCTION evg_ensure_partition(day date)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    part     text := 'detections_' || to_char(day, 'YYYYMMDD');
    start_ts text := to_char(day, 'YYYY-MM-DD') || ' 00:00:00+00';
    end_ts   text := to_char(day + 1, 'YYYY-MM-DD') || ' 00:00:00+00';
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF detections FOR VALUES FROM (%L) TO (%L)',
        part, start_ts, end_ts);
    EXECUTE format(
        'CREATE INDEX IF NOT EXISTS %I ON %I (device_id, captured_at DESC)',
        part || '_device_idx', part);
    EXECUTE format(
        'CREATE INDEX IF NOT EXISTS %I ON %I (label, captured_at DESC)',
        part || '_label_idx', part);
END
$$;
