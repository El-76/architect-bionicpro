CREATE TABLE sensor_measurements (
    measurement_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    prosthesis_id  BIGINT NOT NULL,
    sensor_type    VARCHAR(32) NOT NULL,
    measured_at    TIMESTAMPTZ NOT NULL,
    value          NUMERIC(12, 4) NOT NULL,
    unit           VARCHAR(16) NOT NULL,
    ingested_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT chk_sensor_type
        CHECK (sensor_type IN ('temperature', 'pressure', 'acceleration', 'battery'))
);

CREATE INDEX ix_measurements_measured_at
    ON sensor_measurements (measured_at);
CREATE INDEX ix_measurements_prosthesis_time
    ON sensor_measurements (prosthesis_id, measured_at DESC);

-- Seven days of hourly synthetic telemetry for the five prostheses seeded in Oracle.
-- On a fresh Oracle initialization the generated prosthesis IDs are 1..5.
INSERT INTO sensor_measurements (prosthesis_id, sensor_type, measured_at, value, unit)
SELECT
    p.prosthesis_id,
    s.sensor_type,
    t.measured_at,
    CASE s.sensor_type
        WHEN 'temperature' THEN
            36.5 + 0.8 * sin(extract(epoch FROM t.measured_at) / 18000.0 + p.prosthesis_id)
        WHEN 'pressure' THEN
            210.0 + 18.0 * sin(extract(epoch FROM t.measured_at) / 26000.0 + p.prosthesis_id * 0.7)
        WHEN 'acceleration' THEN
            0.45 + 0.25 * abs(sin(extract(epoch FROM t.measured_at) / 9000.0 + p.prosthesis_id))
        WHEN 'battery' THEN
            greatest(55.0, 98.0 - extract(epoch FROM (now() - t.measured_at)) / 86400.0 * 0.8
                     - p.prosthesis_id * 1.5)
    END,
    CASE s.sensor_type
        WHEN 'temperature' THEN 'C'
        WHEN 'pressure' THEN 'kPa'
        WHEN 'acceleration' THEN 'g'
        WHEN 'battery' THEN 'percent'
    END
FROM generate_series(
    date_trunc('hour', now() - interval '7 days'),
    date_trunc('hour', now()),
    interval '1 hour'
) AS t(measured_at)
CROSS JOIN generate_series(1, 5) AS p(prosthesis_id)
CROSS JOIN (VALUES ('temperature'), ('pressure'), ('acceleration'), ('battery')) AS s(sensor_type);

ANALYZE sensor_measurements;

