#!/bin/sh
# 仅平台 API 在首次启动时负责生成 Ed25519 JWT 密钥；其他服务只读取共享密钥。
set -eu

if [ "${1:-}" = "./api" ] || [ "${1:-}" = "init-keys" ] || [ "${1:-}" = "./init-keys" ]; then
    private_key_path="${AUTH_JWT_PRIVATE_KEY_PATH:-}"
    public_key_path="${AUTH_JWT_PUBLIC_KEY_PATH:-}"

    if [ -n "$private_key_path" ] && [ -n "$public_key_path" ]; then
        if [ ! -f "$private_key_path" ]; then
            if [ -e "$public_key_path" ]; then
                echo 'JWT private key is missing but public key exists; restore the private key instead of rotating identity' >&2
                exit 1
            fi
            mkdir -p "$(dirname "$private_key_path")" "$(dirname "$public_key_path")"
            umask 077
            openssl genpkey -algorithm ED25519 -out "$private_key_path"
            openssl pkey -in "$private_key_path" -pubout -out "$public_key_path"
        elif [ ! -f "$public_key_path" ]; then
            openssl pkey -in "$private_key_path" -pubout -out "$public_key_path"
        fi
        actual_public="$(openssl pkey -in "$private_key_path" -pubout)"
        stored_public="$(openssl pkey -pubin -in "$public_key_path" -pubout)"
        [ "$actual_public" = "$stored_public" ] || { echo 'JWT key pair does not match' >&2; exit 1; }
        chmod 600 "$private_key_path"
        chmod 644 "$public_key_path"
    elif [ "${1:-}" != "./api" ]; then
        echo 'JWT private and public key paths are required for key initialization' >&2
        exit 1
    fi
fi

if [ "${1:-}" = "init-keys" ] || [ "${1:-}" = "./init-keys" ]; then
    exit 0
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

if [ "${1:-}" = "./file-gateway" ] && [ "$(id -u)" = "0" ]; then
    storage_root="${FILE_GATEWAY_STORAGE_ROOT:-/app/data/file-gateway}"
    temporary_root="${FILE_GATEWAY_TEMP_ROOT:-$storage_root/temporary}"
    quarantine_root="${FILE_GATEWAY_QUARANTINE_ROOT:-$storage_root/quarantine}"
    mkdir -p "$storage_root" "$temporary_root" "$quarantine_root"
    chown filegateway:filegateway "$storage_root" "$temporary_root" "$quarantine_root"
    chmod 0750 "$storage_root" "$temporary_root" "$quarantine_root"
    exec su-exec filegateway:filegateway "$@"
fi

if [ "${BASIC_PLATFORM_RUN_WORKER_WITH_API:-false}" = "true" ] && [ "${1:-}" = "./api" ]; then
    run_api_with_worker
fi

exec "$@"
