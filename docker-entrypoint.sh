#!/bin/sh
# 在首次容器启动时生成 Ed25519 JWT 密钥；已有密钥绝不覆盖，便于数据卷持久化。
set -eu

private_key_path="${AUTH_JWT_PRIVATE_KEY_PATH:-}"
public_key_path="${AUTH_JWT_PUBLIC_KEY_PATH:-}"

if [ -n "$private_key_path" ] && [ -n "$public_key_path" ]; then
    if [ ! -f "$private_key_path" ]; then
        mkdir -p "$(dirname "$private_key_path")" "$(dirname "$public_key_path")"
        umask 077
        openssl genpkey -algorithm ED25519 -out "$private_key_path"
        openssl pkey -in "$private_key_path" -pubout -out "$public_key_path"
    elif [ ! -f "$public_key_path" ]; then
        openssl pkey -in "$private_key_path" -pubout -out "$public_key_path"
    fi
fi

run_api_with_worker() {
    ./worker &
    worker_pid=$!
    ./api &
    api_pid=$!

    stop_children() {
        kill -TERM "$api_pid" "$worker_pid" 2>/dev/null || true
    }
    trap stop_children INT TERM HUP

    # 任一进程退出都终止另一个进程，避免容器只剩 API 或只剩后台任务。
    while kill -0 "$api_pid" 2>/dev/null && kill -0 "$worker_pid" 2>/dev/null; do
        sleep 1
    done

    status=0
    if ! kill -0 "$api_pid" 2>/dev/null; then
        set +e
        wait "$api_pid"
        status=$?
        set -e
        kill -TERM "$worker_pid" 2>/dev/null || true
        wait "$worker_pid" 2>/dev/null || true
    else
        set +e
        wait "$worker_pid"
        status=$?
        set -e
        kill -TERM "$api_pid" 2>/dev/null || true
        wait "$api_pid" 2>/dev/null || true
    fi
    exit "$status"
}

if [ "${BASIC_PLATFORM_RUN_WORKER_WITH_API:-false}" = "true" ] && [ "${1:-}" = "./api" ]; then
    run_api_with_worker
fi

exec "$@"
