FROM apache/airflow:2.10.5-python3.11

USER airflow
RUN pip install --no-cache-dir \
    --constraint "https://raw.githubusercontent.com/apache/airflow/constraints-2.10.5/constraints-3.11.txt" \
    apache-airflow-providers-oracle \
    apache-airflow-providers-postgres \
    clickhouse-connect
