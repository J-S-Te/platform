#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
platform_root="$(cd -- "$deploy_dir/../.." && pwd)"
workspace_root="$(cd -- "$platform_root/.." && pwd)"
deploy_script="$script_dir/deploy.sh"
offline_builder="$script_dir/build-offline-packages.sh"
platform_dockerignore="$platform_root/.dockerignore"
data_analysis_dockerfile="$workspace_root/data_analysis/Dockerfile"
workflow="$workspace_root/data_analysis/.github/workflows/ci-cd.yml"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/deployment-parity.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT

for file in "$deploy_script" "$offline_builder"; do
  [[ -f "$file" ]] || { echo "required deployment source is missing: $file" >&2; exit 1; }
done

# In a full workspace, the offline path and CI/CD path must publish the same
# four independently executable data-analysis targets. Standalone platform CI
# still runs the release mapping checks below.
if [[ -d "$workspace_root/data_analysis" ]]; then
  for file in "$data_analysis_dockerfile" "$workflow"; do
    [[ -f "$file" ]] || { echo "required data-analysis deployment source is missing: $file" >&2; exit 1; }
  done
  for target in dashboard-api aggregation-worker alert-worker production-migrate; do
    grep -Fq "target: $target" "$workflow" || {
      echo "CI workflow no longer builds data-analysis target $target" >&2
      exit 1
    }
    grep -Fq "for target in dashboard-api aggregation-worker alert-worker production-migrate" "$offline_builder" || {
      echo "offline builder no longer builds all CI data-analysis targets" >&2
      exit 1
    }
    grep -Fq "AS $target" "$data_analysis_dockerfile" || {
      echo "data-analysis Dockerfile is missing target $target" >&2
      exit 1
    }
  done
fi
grep -Fq 'AS file-gateway-runtime' "$platform_root/Dockerfile" || {
  echo 'Platform Dockerfile is missing its dedicated File Gateway runtime target' >&2
  exit 1
}
for catalog_sync in sync-contract-catalog.sh sync-settlement-catalog.sh sync-project-catalog.sh; do
  grep -Fxq "!scripts/$catalog_sync" "$platform_dockerignore" || {
    echo "platform Docker build context excludes required $catalog_sync" >&2
    exit 1
  }
done

# Every build context is deny-by-default. This keeps local credentials,
# databases, reports and host binaries out of BuildKit uploads/cache even when
# a future Dockerfile accidentally broadens a COPY statement.
for context in platform frontend customer_and_opportunity contract_management project_management Settlement data_analysis; do
  ignore_file="$workspace_root/$context/.dockerignore"
  [[ -f "$ignore_file" ]] || { echo "missing Docker build-context policy: $ignore_file" >&2; exit 1; }
  first_rule="$(awk 'NF && $1 !~ /^#/ {print; exit}' "$ignore_file")"
  [[ "$first_rule" == '**' ]] || {
    echo "$context Docker build context is not deny-by-default" >&2
    exit 1
  }
  case "$context" in
    platform) required_context_rules=('!Dockerfile' '!go.mod' '!go.sum' '!cmd/' '!internal/' '!migrations/' '!scripts/') ;;
    frontend) required_context_rules=('!Dockerfile' '!package.json' '!package-lock.json' '!index.html' '!login.html' '!vite.config.js' '!src/' '!public/' '!nginx/') ;;
    customer_and_opportunity|data_analysis) required_context_rules=('!Dockerfile' '!go.mod' '!go.sum' '!cmd/' '!internal/' '!migrations/') ;;
    contract_management) required_context_rules=('!Dockerfile' '!go.mod' '!go.sum' '!cmd/' '!internal/' '!authz/' '!migrations/' '!docker-entrypoint.sh') ;;
    project_management|Settlement) required_context_rules=('!Dockerfile' '!go.mod' '!go.sum' '!cmd/' '!internal/' '!authz/' '!migrations/') ;;
  esac
  for required in "${required_context_rules[@]}"; do
    grep -Fxq "$required" "$ignore_file" || {
      echo "$context Docker build context does not explicitly allow $required" >&2
      exit 1
    }
  done
  for forbidden in '**/.env' '**/.env.*' '**/*.pem' '**/*.key' '**/*.p12' '**/*.jks' '**/*.bak' '**/*.backup' '**/*.dump' '**/dump*.sql' '**/.DS_Store'; do
    grep -Fxq "$forbidden" "$ignore_file" || {
      echo "$context Docker build context no longer excludes sensitive pattern $forbidden" >&2
      exit 1
    }
  done
done

# Go builders must use the exact toolchain declared by go.mod, retain module
# checksum verification and retry transient proxy failures. Broad COPY of the
# repository root is forbidden for backend production builds.
for context in platform customer_and_opportunity contract_management project_management Settlement data_analysis; do
  dockerfile="$workspace_root/$context/Dockerfile"
  go_mod="$workspace_root/$context/go.mod"
  declared_go="$(awk '$1 == "go" {print $2; exit}' "$go_mod")"
  builder_go="$(awk 'toupper($1) == "FROM" && $2 ~ /^golang:/ {sub(/^golang:/, "", $2); sub(/-alpine.*$/, "", $2); print $2; exit}' "$dockerfile")"
  [[ -n "$declared_go" && "$builder_go" == "$declared_go" ]] || {
    echo "$context Docker Go toolchain ($builder_go) does not match go.mod ($declared_go)" >&2
    exit 1
  }
  grep -Fq 'ARG GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct' "$dockerfile" || {
    echo "$context Dockerfile is missing the overridable resilient GOPROXY chain" >&2
    exit 1
  }
  grep -Fq 'ARG GOSUMDB=sum.golang.google.cn' "$dockerfile" || {
    echo "$context Dockerfile no longer verifies modules through the configured checksum database" >&2
    exit 1
  }
  grep -Fq 'for attempt in 1 2 3 4 5' "$dockerfile" || {
    echo "$context Dockerfile no longer retries transient module downloads" >&2
    exit 1
  }
  grep -Fq 'go mod download && go mod verify' "$dockerfile" || {
    echo "$context Dockerfile no longer verifies downloaded module content" >&2
    exit 1
  }
  if grep -Eq '^COPY[[:space:]]+\.[[:space:]]+\.?/?[[:space:]]*$' "$dockerfile"; then
    echo "$context Dockerfile broadly copies the repository instead of reviewed source roots" >&2
    exit 1
  fi
done

grep -Fq 'npm ci --registry "$NPM_CONFIG_REGISTRY"' "$workspace_root/frontend/Dockerfile" || {
  echo 'frontend Docker build no longer performs lockfile-exact npm installation through an overridable registry' >&2
  exit 1
}
grep -Fq 'target: file-gateway-runtime' "$platform_root/compose.local.yaml" || {
  echo 'local Compose does not run the dedicated File Gateway image' >&2
  exit 1
}
grep -Fq 'target: file-gateway-runtime' "$platform_root/.github/workflows/ci-cd.yml" || {
  echo 'CI/CD does not publish the dedicated File Gateway image' >&2
  exit 1
}
grep -Fq 'manifest_value "$manifest" IMAGE_CONFIG_DIGESTS' "$deploy_script" || {
  echo 'offline importer does not read format-2 image config digests' >&2
  exit 1
}
grep -Fq 'DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE' "$deploy_script" || {
  echo 'offline release mapping is missing a data-analysis runtime image key' >&2
  exit 1
}

# Load deploy.sh definitions without running its CLI, then verify package
# import metadata is copied exactly and not collapsed to the API image.
source "$deploy_script"
release_file="$test_root/.release.env"
cat > "$release_file" <<'EOF'
DATA_ANALYSIS_IMAGE=pending
DATA_ANALYSIS_DASHBOARD_API_IMAGE=pending
DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE=pending
DATA_ANALYSIS_ALERT_WORKER_IMAGE=pending
DATA_ANALYSIS_MIGRATE_IMAGE=pending
EOF
record="$test_root/data-analysis.imported"
cat > "$record" <<'EOF'
IMAGE_REF=127.0.0.1:5000/uip/data-analysis-dashboard-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DATA_ANALYSIS_DASHBOARD_API_IMAGE=127.0.0.1:5000/uip/data-analysis-dashboard-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE=127.0.0.1:5000/uip/data-analysis-aggregation-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
DATA_ANALYSIS_ALERT_WORKER_IMAGE=127.0.0.1:5000/uip/data-analysis-alert-worker@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
DATA_ANALYSIS_MIGRATE_IMAGE=127.0.0.1:5000/uip/data-analysis-production-migrate@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
EOF
stage_data_analysis_images "$record"

for key in DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE; do
  expected="$(awk -F= -v key="$key" '$1 == key {sub(/^[^=]*=/, ""); print; exit}' "$record")"
  actual="$(awk -F= -v key="$key" '$1 == key {sub(/^[^=]*=/, ""); print; exit}' "$release_file")"
  [[ "$actual" == "$expected" ]] || { echo "offline release mapping mismatch for $key" >&2; exit 1; }
done
[[ "$(awk -F= '$1 ~ /^DATA_ANALYSIS_(DASHBOARD_API|AGGREGATION_WORKER|ALERT_WORKER|MIGRATE)_IMAGE$/ {print $2}' "$release_file" | sort -u | wc -l | tr -d ' ')" == 4 ]] || {
  echo 'offline data-analysis services do not retain four distinct image digests' >&2
  exit 1
}

echo 'offline/CI deployment parity tests passed'
