#!/bin/sh
# 平台 API 保留 JWT 首次初始化；凭据 RSA 密钥仅由显式 init-keys 任务创建。
set -eu

# 配置校验必须只读：不生成 JWT 密钥，不启动 API/worker。
if [ "${1:-}" = "./api" ] && [ "${2:-}" = "config-check" ]; then
    exec ./api config-check
fi

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
    # 凭据传输密钥只在显式初始化任务生成。API/config-check 不能隐式换钥。
    case "${CREDENTIAL_ENCRYPTION_MODE:-disabled}" in
        disabled) ;;
        compat|required)
            credential_file="${CREDENTIAL_ENCRYPTION_KEYRING_FILE:-}"
            case "$credential_file" in
                /*.json) ;;
                *) echo 'credential keyring requires an absolute JSON path' >&2; exit 1 ;;
            esac
            [ ! -L "$credential_file" ] || { echo 'credential keyring cannot be a symlink' >&2; exit 1; }
            [ ! -e "$credential_file" ] || [ -f "$credential_file" ] || { echo 'credential keyring must be a regular file' >&2; exit 1; }
            if [ ! -f "$credential_file" ]; then
                credential_dir="$(dirname "$credential_file")"
                [ ! -L "$credential_dir" ] || { echo 'credential key directory cannot be a symlink' >&2; exit 1; }
                mkdir -p "$credential_dir"
                credential_name="$(basename "$credential_file" .json)"
                for credential_existing in "$credential_dir/$credential_name"-*.private.pem "$credential_dir/$credential_name"-*.public.pem; do
                    if [ -e "$credential_existing" ] || [ -L "$credential_existing" ]; then
                        echo 'credential key files exist without metadata; restore the keyring instead of generating a new identity' >&2
                        exit 1
                    fi
                done
                if [ "$(id -u)" = 0 ]; then
                    # 沿用挂载目录属主，使0600私钥可由既有部署账号完整备份。
                    credential_owner="$(stat -c '%u:%g' "$credential_dir")"
                    su-exec "$credential_owner" ./credential-keygen --dir "$credential_dir" --name "$credential_name"
                else
                    ./credential-keygen --dir "$credential_dir" --name "$credential_name"
                fi
            fi
            ;;
        *) echo 'invalid credential encryption mode' >&2; exit 1 ;;
    esac
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
