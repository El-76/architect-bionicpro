-- OLAP schema only.
-- This script creates database objects; it intentionally inserts no business data.
-- Airflow is responsible for loading dimensions and facts.

CREATE SCHEMA IF NOT EXISTS olap;

CREATE TABLE olap.dim_patient (
    patient_id      BIGINT PRIMARY KEY,
    first_name      TEXT NOT NULL,
    last_name       TEXT NOT NULL
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
    measurement_id  BIGINT PRIMARY KEY,
    patient_id      BIGINT NOT NULL REFERENCES olap.dim_patient(patient_id),
    prosthesis_id   BIGINT NOT NULL REFERENCES olap.dim_prosthesis(prosthesis_id),
    sensor_id       BIGINT NOT NULL REFERENCES olap.dim_sensor(sensor_id),
    date_id         INTEGER NOT NULL REFERENCES olap.dim_date(date_id),
    measured_at     TIMESTAMPTZ NOT NULL,
    value           DOUBLE PRECISION NOT NULL
);

CREATE INDEX idx_fact_measurements_patient_date
    ON olap.fact_sensor_measurements (patient_id, date_id);

CREATE INDEX idx_fact_measurements_prosthesis_date
    ON olap.fact_sensor_measurements (prosthesis_id, date_id);

CREATE INDEX idx_fact_measurements_sensor_measured_at
    ON olap.fact_sensor_measurements (sensor_id, measured_at);

ANALYZE;

