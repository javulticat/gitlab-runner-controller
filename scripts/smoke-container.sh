#!/usr/bin/env bash
set -euo pipefail

image=${1:-gitlab-runner-controller:smoke}
root_dir=$(cd "$(dirname "$0")/.." && pwd)
smoke_dir=$(mktemp -d "${TMPDIR:-/tmp}/gitlab-runner-controller-container-smoke-XXXXXXXX")
container="grc-smoke-$(date -u +%Y%m%dT%H%M%S)-$$"
volume="$container-data"
webhook_token=smoke-webhook-0000000000000000000000000000
admin_token=smoke-admin-000000000000000000000000000000

cleanup() {
  result=$?
  if [ "$result" -ne 0 ]; then
    docker logs "$container" >&2 || printf '%s\n' 'Container logs unavailable.' >&2
  fi
  if docker container inspect "$container" >/dev/null 2>&1; then
    docker rm -f "$container" >/dev/null
  fi
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    docker volume rm "$volume" >/dev/null
  fi
  for artifact in "$smoke_dir"/*; do
    if [ -f "$artifact" ]; then rm "$artifact"; fi
  done
  rmdir "$smoke_dir"
  exit "$result"
}
trap cleanup EXIT

config=$(jq -c '.gitlab_url="https://127.0.0.1:1"' "$root_dir/controller.example.json")
docker volume create "$volume" >/dev/null
docker run -d --name "$container" --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true --mount "type=volume,src=$volume,dst=/data" \
  -p 127.0.0.1::3000 -e PORT=3000 -e STORAGE_DIR=/data \
  -e "CONTROLLER_CONFIG_JSON=$config" -e GITLAB_API_TOKEN=smoke-api-token \
  -e "GITLAB_WEBHOOK_SECRET=$webhook_token" -e "ADMIN_TOKEN=$admin_token" \
  -e 'HOST_A_COMMAND_JSON=["/opt/provider-fixture"]' -e HOST_A_ENDPOINT=https://127.0.0.1:1 \
  -e HOST_A_TOKEN=smoke-provider-token -e HOST_A_ACCOUNT_ID=account-example -e HOST_A_LIFECYCLE=retained \
  -e GITLAB_RUNNER_LINUX_TOKEN=glrt-smoke-linux \
  -e GITLAB_RUNNER_PROTECTED_TOKEN=glrt-smoke-protected "$image" >"$smoke_dir/container-id"
address="http://$(docker port "$container" 3000/tcp)"

wait_for_health() {
  address="http://$(docker port "$container" 3000/tcp)"
  for _ in {1..50}; do
    if curl -fsS "$address/healthz" >/dev/null 2>&1; then return; fi
    sleep 0.1
  done
  printf '%s\n' 'Container did not become healthy.' >&2
  return 1
}

wait_for_health
test "$(docker inspect -f '{{.Config.User}}' "$container")" = 65532:65532
test "$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "$container")" = true
docker exec "$container" /usr/local/bin/gitlab-runner-controller healthcheck
jq -nc '{version:1,id:"fixture-describe",operation:"describe",expected:{identity:"account-example",lifecycle:"retained"},deadline:(now+60|todateiso8601)}' >"$smoke_dir/request.json"
docker exec -i -e ACCOUNT_ID=account-example -e LIFECYCLE=retained "$container" /opt/provider-fixture <"$smoke_dir/request.json" >"$smoke_dir/response.json"
jq -e '.version == 1 and .id == "fixture-describe" and .description.identity == "account-example" and .description.scope == "fixture-scope" and .error == null' "$smoke_dir/response.json" >/dev/null
test "$(curl -sS -o /dev/null -w '%{http_code}' "$address/readyz")" = 503
test "$(curl -sS -o /dev/null -w '%{http_code}' "$address/status")" = 401
curl -fsS -H "Authorization: Bearer $admin_token" "$address/status" >"$smoke_dir/status.json"
jq -e '.state.version == 1 and .ready == false' "$smoke_dir/status.json" >/dev/null
curl -fsS -H "Authorization: Bearer $admin_token" "$address/metrics" >"$smoke_dir/metrics"
grep -q '^grc_reconciliations_total' "$smoke_dir/metrics"

if docker exec "$container" /usr/local/bin/gitlab-runner-controller serve >"$smoke_dir/singleton.log" 2>&1; then
  printf '%s\n' 'Second controller unexpectedly acquired storage.' >&2
  exit 1
fi
grep -q 'another controller owns this state file' "$smoke_dir/singleton.log"

test "$(curl -sS -o /dev/null -w '%{http_code}' -H "X-Gitlab-Token: $webhook_token" \
  -H 'X-Gitlab-Event: Job Hook' -H 'Content-Type: application/json' \
  --data '{"object_kind":"build","project_id":123,"build_id":77,"pipeline_id":9,"build_status":"pending"}' \
  "$address/webhook/gitlab")" = 202
docker cp "$container:/data/state.json" "$smoke_dir/before.json"
jq -e '.hint_revision == 1 and .hints["123/77"].status == "pending"' "$smoke_dir/before.json" >/dev/null

docker stop --time 30 "$container" >/dev/null
test "$(docker inspect -f '{{.State.ExitCode}}' "$container")" = 0
docker start "$container" >/dev/null
wait_for_health
docker cp "$container:/data/state.json" "$smoke_dir/after.json"
jq -e '.hint_revision == 1 and .hints["123/77"].status == "pending"' "$smoke_dir/after.json" >/dev/null
docker stop --time 30 "$container" >/dev/null
test "$(docker inspect -f '{{.State.ExitCode}}' "$container")" = 0
printf '%s\n' 'Container smoke test passed.'
