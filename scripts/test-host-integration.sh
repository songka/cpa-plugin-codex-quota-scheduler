#!/usr/bin/env bash
set -euo pipefail
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
make -C "$project_dir" build BUILD_DIR=dist/integration
host_dir=${CPA_HOST_CHECKOUT:-}
owned_host=false
state_dir=$(mktemp -d)
fixture=
registry_fixture=
settings_fixture=
cleanup() {
  [[ -z "$fixture" ]] || rm -f -- "$fixture"
  [[ -z "$registry_fixture" ]] || rm -f -- "$registry_fixture"
  [[ -z "$settings_fixture" ]] || rm -f -- "$settings_fixture"
  rm -rf -- "$state_dir"
  if [[ "$owned_host" == true ]]; then rm -rf -- "$host_dir"; fi
}
trap cleanup EXIT
if [[ -z "$host_dir" ]]; then
  host_dir=$(mktemp -d)
  owned_host=true
  git clone --depth 1 --branch v8.0.27 https://github.com/router-for-me/CLIProxyAPI.git "$host_dir"
fi
[[ $(git -C "$host_dir" rev-parse HEAD) == bf9341629ef2300ac9d846a2e80fe2756f63a671 ]]
candidate_fixture="$host_dir/internal/pluginhost/reserve_external_test.go"
[[ ! -e "$candidate_fixture" ]] || { echo 'Refusing to overwrite an existing integration fixture' >&2; exit 1; }
fixture=$candidate_fixture
cp "$project_dir/tests/integration/reserve_host_test.go.txt" "$fixture"
XDG_CONFIG_HOME="$state_dir" RESERVE_LIBRARY="$project_dir/dist/integration/codex-quota-reserve.so" go -C "$host_dir" test -v ./internal/pluginhost -run '^TestReserveDynamicHostIntegration$' -count=1
go -C "$host_dir" test ./sdk/cliproxy/auth -run '^TestManagerPluginSchedulerTerminalRejection$' -count=1

candidate_registry="$host_dir/internal/pluginstore/reserve_registry_external_test.go"
[[ ! -e "$candidate_registry" ]] || { echo 'Refusing to overwrite an existing registry fixture' >&2; exit 1; }
registry_fixture=$candidate_registry
cp "$project_dir/tests/integration/reserve_registry_test.go.txt" "$registry_fixture"
RESERVE_REGISTRY="$project_dir/registry.json" go -C "$host_dir" test ./internal/pluginstore -run '^TestReserveRegistrySchema$' -count=1

candidate_settings="$host_dir/internal/api/handlers/management/reserve_settings_external_test.go"
[[ ! -e "$candidate_settings" ]] || { echo 'Refusing to overwrite an existing settings fixture' >&2; exit 1; }
settings_fixture=$candidate_settings
cp "$project_dir/tests/integration/reserve_settings_test.go.txt" "$settings_fixture"
go -C "$host_dir" test ./internal/api/handlers/management -run '^TestReserveSettingsPatchPersists$' -count=1
