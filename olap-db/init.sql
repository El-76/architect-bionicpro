CREATE SCHEMA IF NOT EXISTS olap;

CREATE TABLE olap.dim_patient (
    patient_id          BIGINT PRIMARY KEY,
    patient_identity_id TEXT NOT NULL,
    first_name          TEXT NOT NULL,
    last_name           TEXT NOT NULL
);

CREATE TABLE olap.dim_prosthesis (
    prosthesis_id   BIGINT PRIMARY KEY,
    model           TEXT NOT NULL,
    serial_number   TEXT NOT NULL UNIQUE
);

CREATE TABLE olap.dim_sensor (
    sensor_id       BIGINT PRIMARY KEY,
    sensor_type     TEXT NOT NULL UNIQUE,
    unit            TEXT NOT NULL
);

CREATE TABLE olap.dim_date (
    date_id         INTEGER PRIMARY KEY, -- YYYYMMDD
    date_value      DATE NOT NULL UNIQUE,
    day_of_month    SMALLINT NOT NULL,
    month_number    SMALLINT NOT NULL,
    year_number     SMALLINT NOT NULL
);

CREATE TABLE olap.fact_sensor_measurements (
    measurement_id      BIGINT PRIMARY KEY,
    patient_id          BIGINT NOT NULL REFERENCES olap.dim_patient(patient_id),
    patient_identity_id TEXT NOT NULL,
    prosthesis_id       BIGINT NOT NULL REFERENCES olap.dim_prosthesis(prosthesis_id),
    sensor_id           BIGINT NOT NULL REFERENCES olap.dim_sensor(sensor_id),
    date_id             INTEGER NOT NULL REFERENCES olap.dim_date(date_id),
    measured_at         TIMESTAMPTZ NOT NULL,
    value               DOUBLE PRECISION NOT NULL
);

CREATE INDEX idx_fact_measurements_patient_date
    ON olap.fact_sensor_measurements (patient_id, date_id);

CREATE INDEX idx_fact_measurements_prosthesis_date
    ON olap.fact_sensor_measurements (prosthesis_id, date_id);

CREATE INDEX idx_fact_measurements_sensor_measured_at
    ON olap.fact_sensor_measurements (sensor_id, measured_at);

-- Reporting mart: one row per patient, prosthesis, day and sensor type.
-- CRM and prosthesis attributes are denormalized here for fast report reads.
CREATE TABLE olap.mart_patient_telemetry_daily (
    patient_id          BIGINT NOT NULL REFERENCES olap.dim_patient(patient_id),
    patient_identity_id TEXT NOT NULL,
    first_name          TEXT NOT NULL,
    last_name           TEXT NOT NULL,

    prosthesis_id       BIGINT NOT NULL REFERENCES olap.dim_prosthesis(prosthesis_id),
    prosthesis_model    TEXT NOT NULL,
    serial_number       TEXT NOT NULL,

    date_id             INTEGER NOT NULL REFERENCES olap.dim_date(date_id),
    measurement_date    DATE NOT NULL,

    sensor_id           BIGINT NOT NULL REFERENCES olap.dim_sensor(sensor_id),
    sensor_type         TEXT NOT NULL,
    unit                TEXT NOT NULL,

    measurement_count   BIGINT NOT NULL,
    avg_value           DOUBLE PRECISION NOT NULL,
    min_value           DOUBLE PRECISION NOT NULL,
    max_value           DOUBLE PRECISION NOT NULL,

    PRIMARY KEY (patient_id, prosthesis_id, date_id, sensor_id)
);

-- Main report access pattern: retrieve a patient's telemetry over time.
CREATE INDEX idx_mart_patient_date
    ON olap.mart_patient_telemetry_daily (patient_id, measurement_date DESC);

-- Useful when a report filters one patient's measurements by sensor type.
CREATE INDEX idx_mart_patient_sensor_date
    ON olap.mart_patient_telemetry_daily (patient_id, sensor_type, measurement_date DESC);

ANALYZE;

