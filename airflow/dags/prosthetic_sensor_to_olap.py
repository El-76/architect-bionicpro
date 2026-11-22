from datetime import date, datetime, timedelta
import logging

import pendulum
from airflow import DAG
from airflow.operators.python import PythonOperator
from airflow.providers.oracle.hooks.oracle import OracleHook
from airflow.providers.postgres.hooks.postgres import PostgresHook


DAG_ID = "prosthesis_sensor_to_olap"

# Stable surrogate keys for the four sensor types in this lab.
SENSOR_IDS = {
    "temperature": 1,
    "pressure": 2,
    "acceleration": 3,
    "battery": 4,
}


def _date_key(value: datetime) -> tuple[int, date]:
    day = value.date()
    return int(day.strftime("%Y%m%d")), day


def load_dimensions_and_facts() -> None:
    """Incrementally load source data, then rebuild the reporting mart."""

    # 1. Read CRM dimensions from Oracle.
    oracle_hook = OracleHook(oracle_conn_id="source_oracle")
    with oracle_hook.get_conn() as oracle_conn:
        with oracle_conn.cursor() as cur:
            cur.execute("""
                SELECT patient_id, patient_identity_id, first_name, last_name
                FROM CRM.PATIENTS
                ORDER BY patient_id
            """)
            patients = cur.fetchall()

            cur.execute("""
                SELECT prosthesis_id, patient_id, model, serial_number
                FROM CRM.PROSTHESES
                ORDER BY prosthesis_id
            """)
            prostheses = cur.fetchall()

    patient_ids = {row[0] for row in patients}
    patient_id_to_identity_id = {row[0]: row[1] for row in patients}
    prosthesis_to_patient = {row[0]: row[1] for row in prostheses}

    for prosthesis_id, patient_id, _model, _serial in prostheses:
        if patient_id not in patient_ids:
            raise ValueError(
                f"Prosthesis {prosthesis_id} references missing patient {patient_id}"
            )

    # 2. Find the current OLAP watermark.
    olap_hook = PostgresHook(postgres_conn_id="olap_postgres")
    with olap_hook.get_conn() as olap_conn:
        with olap_conn.cursor() as cur:
            cur.execute("""
                SELECT COALESCE(MAX(measurement_id), 0)
                FROM olap.fact_sensor_measurements
            """)
            last_measurement_id = cur.fetchone()[0]

    # 3. Extract only new telemetry rows from the source PostgreSQL.
    source_hook = PostgresHook(postgres_conn_id="source_telemetry")
    with source_hook.get_conn() as source_conn:
        with source_conn.cursor() as cur:
            cur.execute("""
                SELECT measurement_id, prosthesis_id, sensor_type,
                       measured_at, value, unit
                FROM sensor_measurements
                WHERE measurement_id > %s
                ORDER BY measurement_id
            """, (last_measurement_id,))
            measurements = cur.fetchall()

    # 4. Validate sensor types and units in this incremental batch.
    sensor_units: dict[str, str] = {}
    for (
        _measurement_id,
        _prosthesis_id,
        sensor_type,
        _measured_at,
        _value,
        unit,
    ) in measurements:
        if sensor_type not in SENSOR_IDS:
            raise ValueError(f"Unsupported sensor_type: {sensor_type}")

        previous_unit = sensor_units.setdefault(sensor_type, unit)
        if previous_unit != unit:
            raise ValueError(
                f"Inconsistent units for sensor_type={sensor_type}: "
                f"{previous_unit!r} and {unit!r}"
            )

    sensor_rows = [
        (SENSOR_IDS[sensor_type], sensor_type, unit)
        for sensor_type, unit in sorted(sensor_units.items())
    ]

    date_rows: dict[int, tuple[int, date]] = {}
    fact_rows = []

    for measurement_id, prosthesis_id, sensor_type, measured_at, value, _unit in measurements:
        if prosthesis_id not in prosthesis_to_patient:
            raise ValueError(
                f"Measurement {measurement_id} references unknown prosthesis {prosthesis_id}"
            )
        if sensor_type not in SENSOR_IDS:
            raise ValueError(f"Unsupported sensor_type: {sensor_type}")

        date_id, date_value = _date_key(measured_at)
        date_rows[date_id] = (date_id, date_value)

        patient_id = prosthesis_to_patient[prosthesis_id]

        fact_rows.append((
            measurement_id,
            patient_id,
            patient_id_to_identity_id[patient_id],
            prosthesis_id,
            SENSOR_IDS[sensor_type],
            date_id,
            measured_at,
            float(value),
        ))

    # 5. Load dimensions and facts, then rebuild the reporting mart.
    # Everything in this block is committed as one OLAP transaction.
    with olap_hook.get_conn() as olap_conn:
        with olap_conn.cursor() as cur:
            cur.executemany("""
                INSERT INTO olap.dim_patient (patient_id, patient_identity_id, first_name, last_name)
                VALUES (%s, %s, %s, %s)
                ON CONFLICT (patient_id) DO UPDATE SET
                    patient_identity_id = EXCLUDED.patient_identity_id,
                    first_name = EXCLUDED.first_name,
                    last_name = EXCLUDED.last_name
            """, patients)

            cur.executemany("""
                INSERT INTO olap.dim_prosthesis
                    (prosthesis_id, model, serial_number)
                VALUES (%s, %s, %s)
                ON CONFLICT (prosthesis_id) DO UPDATE SET
                    model = EXCLUDED.model,
                    serial_number = EXCLUDED.serial_number
            """, [
                (prosthesis_id, model, serial_number)
                for prosthesis_id, _patient_id, model, serial_number in prostheses
            ])

            cur.executemany("""
                INSERT INTO olap.dim_sensor (sensor_id, sensor_type, unit)
                VALUES (%s, %s, %s)
                ON CONFLICT (sensor_id) DO UPDATE SET
                    sensor_type = EXCLUDED.sensor_type,
                    unit = EXCLUDED.unit
            """, sensor_rows)

            cur.executemany("""
                INSERT INTO olap.dim_date
                    (date_id, date_value, day_of_month, month_number, year_number)
                VALUES (%s, %s, %s, %s, %s)
                ON CONFLICT (date_id) DO UPDATE SET
                    date_value = EXCLUDED.date_value,
                    day_of_month = EXCLUDED.day_of_month,
                    month_number = EXCLUDED.month_number,
                    year_number = EXCLUDED.year_number
            """, [
                (
                    date_id,
                    day,
                    day.day,
                    day.month,
                    day.year,
                )
                for date_id, day in date_rows.values()
            ])

            if fact_rows:
                cur.executemany("""
                    INSERT INTO olap.fact_sensor_measurements
                        (measurement_id, patient_id, patient_identity_id, prosthesis_id, sensor_id,
                         date_id, measured_at, value)
                    VALUES (%s, %s, %s, %s, %s, %s, %s, %s)
                    ON CONFLICT (measurement_id) DO UPDATE SET
                        patient_id = EXCLUDED.patient_id,
                        patient_identity_id = EXCLUDED.patient_identity_id,
                        prosthesis_id = EXCLUDED.prosthesis_id,
                        sensor_id = EXCLUDED.sensor_id,
                        date_id = EXCLUDED.date_id,
                        measured_at = EXCLUDED.measured_at,
                        value = EXCLUDED.value
                """, fact_rows)

            # The mart is a derived aggregate, so rebuild it from the complete
            # fact table. This also reflects CRM dimension changes on every run.
            cur.execute("DELETE FROM olap.mart_patient_telemetry_daily")

            cur.execute("""
                INSERT INTO olap.mart_patient_telemetry_daily (
                    patient_id,
                    patient_identity_id,
                    first_name,
                    last_name,
                    prosthesis_id,
                    prosthesis_model,
                    serial_number,
                    date_id,
                    measurement_date,
                    sensor_id,
                    sensor_type,
                    unit,
                    measurement_count,
                    avg_value,
                    min_value,
                    max_value
                )
                SELECT
                    p.patient_id,
                    p.patient_identity_id,
                    p.first_name,
                    p.last_name,
                    pr.prosthesis_id,
                    pr.model,
                    pr.serial_number,
                    d.date_id,
                    d.date_value,
                    s.sensor_id,
                    s.sensor_type,
                    s.unit,
                    COUNT(*) AS measurement_count,
                    AVG(f.value) AS avg_value,
                    MIN(f.value) AS min_value,
                    MAX(f.value) AS max_value
                FROM olap.fact_sensor_measurements f
                JOIN olap.dim_patient p
                  ON p.patient_id = f.patient_id
                JOIN olap.dim_prosthesis pr
                  ON pr.prosthesis_id = f.prosthesis_id
                JOIN olap.dim_date d
                  ON d.date_id = f.date_id
                JOIN olap.dim_sensor s
                  ON s.sensor_id = f.sensor_id
                GROUP BY
                    p.patient_id,
                    p.first_name,
                    p.last_name,
                    pr.prosthesis_id,
                    pr.model,
                    pr.serial_number,
                    d.date_id,
                    d.date_value,
                    s.sensor_id,
                    s.sensor_type,
                    s.unit
            """)

            mart_rows = cur.rowcount

    logging.info(
        "OLAP load completed: patients=%d prostheses=%d "
        "new_measurements=%d mart_rows=%d",
        len(patients),
        len(prostheses),
        len(fact_rows),
        mart_rows,
    )


with DAG(
    dag_id=DAG_ID,
    description=(
        "Load CRM dimensions and sensor telemetry into OLAP, "
        "then refresh the patient daily telemetry reporting mart"
    ),
    start_date=pendulum.datetime(2026, 1, 1, tz="UTC"),
    schedule="@daily",
    catchup=False,
    max_active_runs=1,
    default_args={
        "owner": "etl",
        "retries": 2,
        "retry_delay": timedelta(minutes=2),
    },
    tags=["prosthesis", "etl", "olap", "reporting-mart"],
) as dag:
    load_olap = PythonOperator(
        task_id="load_dimensions_and_facts",
        python_callable=load_dimensions_and_facts,
    )

