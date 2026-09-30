#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source_script="$script_dir/lifecycle.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-lifecycle-test.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

mkdir -p "$test_root/bin"
cat >"$test_root/bin/docker" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$DOCKER_CALL_LOG"
exit 0
MOCK
chmod +x "$test_root/bin/docker"

export PATH="$test_root/bin:$PATH"
export DOCKER_CALL_LOG="$test_root/docker.log"
: >"$DOCKER_CALL_LOG"

if bash "$source_script" remove contract --confirm WRONG >/dev/null 2>&1; then
  echo 'wrong remove confirmation unexpectedly succeeded' >&2
  exit 1
fi
[[ ! -s "$DOCKER_CALL_LOG" ]] || { echo 'wrong confirmation reached Docker' >&2; exit 1; }

if bash "$source_script" cleanup --confirm WRONG >/dev/null 2>&1; then
  echo 'wrong cleanup confirmation unexpectedly succeeded' >&2
  exit 1
fi
[[ ! -s "$DOCKER_CALL_LOG" ]] || { echo 'wrong cleanup confirmation reached Docker' >&2; exit 1; }
if bash "$source_script" destroy --confirm WRONG >/dev/null 2>&1; then
  echo 'wrong destroy confirmation unexpectedly succeeded' >&2
  exit 1
fi
[[ ! -s "$DOCKER_CALL_LOG" ]] || { echo 'wrong destroy confirmation reached Docker' >&2; exit 1; }

help_output="$(bash "$source_script" help)"
for action in 'repair <platform|frontend|子系统|all>' 'remove <子系统>' 'remove-all-applications' 'cleanup --confirm' 'purge --keep-data' 'destroy --confirm'; do
  [[ "$help_output" == *"$action"* ]] || { printf 'missing lifecycle help item: %s\n' "$action" >&2; exit 1; }
done

# Ordinary removal paths must retain data volumes. Full destroy is isolated behind
# its own command, requires backup checks, and only removes project-labelled volumes.
grep -Fq 'docker rm --force "$id"' "$source_script" || { echo 'container removal is not scoped by exact ID' >&2; exit 1; }
grep -Fq 'label=com.docker.compose.project=$project' "$source_script" || { echo 'Docker cleanup is not project-scoped' >&2; exit 1; }
grep -Fq 'service=platform-api' "$source_script" || { echo 'platform repair does not target the actual Compose service' >&2; exit 1; }
grep -Fq 'repair 不会替代首次部署或平台受控接入' "$source_script" || { echo 'subsystem repair can no longer distinguish repair from first deployment' >&2; exit 1; }
grep -Fq '== "$backup_started" ]] ||' "$source_script" || { echo 'destroy does not require a fresh system backup' >&2; exit 1; }
grep -Fq '"$deploy_dir/bin/backup-all.sh"' "$source_script" || { echo 'destroy does not create a verified backup first' >&2; exit 1; }
grep -Fq '"$deploy_dir/bin/backup-all.sh" --deployment-lock-fd 8' "$source_script" || { echo 'destroy backup does not inherit the held deployment lock' >&2; exit 1; }
grep -Fq 'docker volume rm "${volumes[@]}"' "$source_script" || { echo 'destroy does not remove only collected project volumes' >&2; exit 1; }
grep -Fq 'rm -rf -- "$deploy_dir"' "$source_script" || { echo 'purge no longer removes its guarded deployment root' >&2; exit 1; }
grep -Fq 'destroy --confirm DELETE_UIP_DATA' "$source_script" || { echo 'destroy confirmation is not isolated behind its dedicated token' >&2; exit 1; }

echo 'deployment lifecycle safety checks passed'
