#!/bin/sh
set -eu

storage_root="${FILE_GATEWAY_STORAGE_ROOT:-/app/data/file-gateway}"
temporary_root="${FILE_GATEWAY_TEMP_ROOT:-$storage_root/temporary}"
quarantine_root="${FILE_GATEWAY_QUARANTINE_ROOT:-$storage_root/quarantine}"
mkdir -p "$storage_root" "$temporary_root" "$quarantine_root"
chown filegateway:filegateway "$storage_root" "$temporary_root" "$quarantine_root"
chmod 0750 "$storage_root" "$temporary_root" "$quarantine_root"
exec su-exec filegateway:filegateway "$@"
