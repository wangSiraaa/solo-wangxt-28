#!/usr/bin/env bash
# 一键启动本地开发环境：PostgreSQL + MinIO + Go/Gin 配额服务 + Next.js
# 所有组件均为用户态安装（无需 root / docker），二进制位于 /workspace/.tools 与 /workspace/dl
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
PGSQL="/workspace/.tools/pgsql"
PGBIN="$PGSQL/usr/lib/postgresql/15/bin"
export LD_LIBRARY_PATH="$PGSQL/usr/lib/postgresql/15/lib:$PGSQL/usr/lib/aarch64-linux-gnu:${LD_LIBRARY_PATH:-}"
export PATH="/workspace/.tools/go/bin:$PATH"

PGDATA=/workspace/.data/pgdata
RUN=/tmp/quotaapp
mkdir -p "$RUN" /workspace/.data/minio

wait_url() { # url times
  local url="$1" times="${2:-30}"
  for _ in $(seq 1 "$times"); do
    if curl -sf -o /dev/null --max-time 2 "$url"; then return 0; fi
    sleep 1
  done
  echo "等待 $url 超时"; return 1
}

# 1) PostgreSQL（首次自动 initdb）
if ! pgrep -x postgres >/dev/null 2>&1; then
  if [ ! -f "$PGDATA/PG_VERSION" ]; then
    mkdir -p "$PGDATA"
    "$PGBIN/initdb" -D "$PGDATA" -U quota --auth=trust --no-locale -E UTF8
    printf "port = 55432\nunix_socket_directories = '/tmp'\nlisten_addresses = '127.0.0.1'\nmax_connections = 50\nshared_buffers = 64MB\n" >> "$PGDATA/postgresql.conf"
  fi
  "$PGBIN/pg_ctl" -D "$PGDATA" -l "$RUN/postgres.log" start
  wait_url "http://127.0.0.1:1" 1 >/dev/null 2>&1 || true
  for _ in $(seq 1 20); do "$PGBIN/psql" -h 127.0.0.1 -p 55432 -U quota -d postgres -c "SELECT 1" >/dev/null 2>&1 && break; sleep 1; done
fi
"$PGBIN/psql" -h 127.0.0.1 -p 55432 -U quota -d postgres -tc "SELECT 1 FROM pg_database WHERE datname='quota_ledger'" | grep -q 1 \
  || "$PGBIN/psql" -h 127.0.0.1 -p 55432 -U quota -d postgres -c "CREATE DATABASE quota_ledger;"

# 2) MinIO
if ! curl -sf -o /dev/null http://127.0.0.1:9000/minio/health/live; then
  (MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_BROWSER=off \
    /workspace/dl/minio server /workspace/.data/minio --address 127.0.0.1:9000 \
    > "$RUN/minio.log" 2>&1 &)
  wait_url http://127.0.0.1:9000/minio/health/live 30
fi

# 3) Go 配额服务
if ! curl -sf -o /dev/null http://127.0.0.1:8080/health; then
  (cd "$ROOT/backend" && SESSION_TTL_SECONDS=30 SWEEP_INTERVAL_MS=2000 GIN_MODE=release \
    go run ./cmd/server > "$RUN/server.log" 2>&1 &)
  wait_url http://127.0.0.1:8080/health 30
fi

# 4) Next.js 前端
if ! curl -sf -o /dev/null http://127.0.0.1:3000/; then
  if [ ! -d "$ROOT/web/node_modules" ]; then
    (cd "$ROOT/web" && npm install --no-audit --no-fund)
  fi
  (cd "$ROOT/web" && npx next start -p 3000 > "$RUN/web.log" 2>&1 &)
  wait_url http://127.0.0.1:3000/ 40
fi

cat <<EOF

✅ 环境已就绪
   前端（管理界面）  http://127.0.0.1:3000
   配额服务 API     http://127.0.0.1:8080   (GET /health)
   MinIO S3         http://127.0.0.1:9000   (凭据仅服务端持有)
   PostgreSQL       127.0.0.1:55432/quota_ledger

   复现全部场景：  python3 $ROOT/scripts/scenario_test.py
   日志目录：      $RUN/
EOF
