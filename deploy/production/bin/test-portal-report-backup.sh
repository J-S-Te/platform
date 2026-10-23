#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/portal-report-backup.sh"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/uip-portal-backup.XXXXXX")"
trap 'rm -rf -- "$fixture"' EXIT
mkdir "$fixture/source" "$fixture/output"
printf 'encrypted-report-fixture' > "$fixture/source/owned.report"
printf 'CUSTOMER_PORTAL_IMAGE=local/portal@sha256:%064d\n' 1 > "$fixture/release.env"
volume_output=qa_portal-reports-data
docker() {
  if [[ "$1" == volume ]]; then printf '%s' "$volume_output"; return; fi
  [[ "$*" == *'--network none --read-only --cap-drop ALL'* ]] || return 91
  [[ "$*" == *'type=volume,source=qa_portal-reports-data,target=/reports,readonly'* ]] || return 92
  [[ "$*" != *'-e '* && "$*" != *'--env'* ]] || return 93
  tar -cf - -C "$fixture/source" . | gzip -c
}
backup_portal_reports qa "$fixture/release.env" "$fixture/output"
gzip -t "$fixture/output/portal-reports.tar.gz"
tar -xzf "$fixture/output/portal-reports.tar.gz" -C "$fixture/output"
cmp "$fixture/source/owned.report" "$fixture/output/owned.report"
volume_output=$'qa_one\nqa_two'
if backup_portal_reports qa "$fixture/release.env" "$fixture/output" 2>/dev/null; then exit 1; fi
volume_output=qa_portal-reports-data
printf 'CUSTOMER_PORTAL_IMAGE=local/portal:latest\n' > "$fixture/release.env"
if backup_portal_reports qa "$fixture/release.env" "$fixture/output" 2>/dev/null; then exit 1; fi
volume_output=''
backup_portal_reports qa "$fixture/release.env" "$fixture/output"
echo 'Portal immutable vault backup, read-only isolation and retained-volume rejection passed'
