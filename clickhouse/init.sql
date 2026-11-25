CREATE DATABASE IF NOT EXISTS olap;

-- Dimensions use ReplacingMergeTree so refreshed CRM attributes replace older versions.
-- FINAL is used by the ETL aggregation query to read the latest dimension versions.
CREATE TABLE IF NOT EXISTS olap.dim_patient
(
    patient_id          UInt64,
    patient_identity_id String,
    first_name          String,
    last_name           String,
    updated_at          DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY patient_id;

CREATE TABLE IF NOT EXISTS olap.dim_prosthesis
(
    prosthesis_id UInt64,
    model         String,
    serial_number String,
    updated_at    DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY prosthesis_id;

CREATE TABLE IF NOT EXISTS olap.dim_sensor
(
    sensor_id   UInt8,
    sensor_type LowCardinality(String),
    unit        LowCardinality(String),
    updated_at  DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY sensor_id;

CREATE TABLE IF NOT EXISTS olap.dim_date
(
    date_id      UInt32,
    date_value   Date,
    day_of_month UInt8,
    month_number UInt8,
    year_number  UInt16,
    updated_at   DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY date_id;

-- Telemetry is append-only; measurement_id is the incremental watermark.
CREATE TABLE IF NOT EXISTS olap.fact_sensor_measurements
(
    measurement_id      UInt64,
    patient_id          UInt64,
    patient_identity_id String,
    prosthesis_id       UInt64,
    sensor_id           UInt8,
    date_id             UInt32,
    measured_at         DateTime64(6, 'UTC'),
    value               Float64
)
ENGINE = MergeTree
ORDER BY (measurement_id);

-- Denormalized daily aggregate used by the reporting API.
CREATE TABLE IF NOT EXISTS olap.mart_patient_telemetry_daily
(
    patient_id          UInt64,
    patient_identity_id String,
    first_name          String,
    last_name           String,
    prosthesis_id       UInt64,
    prosthesis_model    String,
    serial_number       String,
    date_id             UInt32,
    measurement_date    Date,
    sensor_id           UInt8,
    sensor_type         LowCardinality(String),
    unit                LowCardinality(String),
    measurement_count   UInt64,
    avg_value           Float64,
    min_value           Float64,
    max_value           Float64
)
ENGINE = MergeTree
ORDER BY (patient_id, measurement_date, prosthesis_id, sensor_id);
