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

exec "$@"
