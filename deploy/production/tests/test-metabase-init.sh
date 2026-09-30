#!/usr/bin/env bash
set -Eeuo pipefail
sql="$(cd "$(dirname "$0")/../mysql-init" && pwd)/data-analysis-metabase.sql"
export MYSQL_ROOT_PASSWORD="$(openssl rand -hex 24)"
export MYSQL_PASSWORD="$(openssl rand -hex 24)"
container="$(docker run -d --network none --label uip.test=metabase-init \
  --tmpfs /var/lib/mysql -e MYSQL_ROOT_PASSWORD -e MYSQL_PASSWORD \
  -e MYSQL_DATABASE=dashboard_aggregation -e MYSQL_USER=dashboard mysql:8.4)"
unset MYSQL_ROOT_PASSWORD MYSQL_PASSWORD
trap 'docker rm -f "$container" >/dev/null' EXIT
ready=false
for ((attempt=0; attempt<120; attempt++)); do
  if docker exec "$container" sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -h127.0.0.1 -uroot -Nse "SELECT 1"' >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
[[ "$ready" == true ]] || { echo 'isolated MySQL readiness timed out' >&2; exit 1; }
for attempt in 1 2; do
  docker exec -i "$container" sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -h127.0.0.1 -uroot' < "$sql"
done
docker exec "$container" sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -h127.0.0.1 -udashboard -Ddashboard_metabase -e "CREATE TABLE init_fixture (id INT PRIMARY KEY); INSERT INTO init_fixture VALUES (1); SELECT id FROM init_fixture;"' >/dev/null
echo 'PASS: existing MySQL initialized twice; Metabase account can create, insert and read'
