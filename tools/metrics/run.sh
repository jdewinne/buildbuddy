#!/bin/bash

set -e -o pipefail
__file__="$0"
__dir__=$(dirname "$__file__")

cd "$__dir__"

: ${GRAFANA_PORT:=4500}
: ${GRAFANA_ADMIN_PASSWORD:="admin"}
GRAFANA_STARTUP_URL="http://localhost:$GRAFANA_PORT?orgId=1&refresh=5s"
GRAFANA_DASHBOARD_URL="http://admin:$GRAFANA_ADMIN_PASSWORD@localhost:$GRAFANA_PORT/api/dashboards/db/buildbuddy-metrics"
GRAFANA_DASHBOARD_FILE_PATH="./grafana/dashboards/buildbuddy.json"

: ${KUBE_CONTEXT:=""}
: ${KUBE_NAMESPACE:="monitor-dev"}
: ${KUBE_PROM_SERVER_RESOURCE:="deployment/prometheus-server"}
: ${KUBE_PROM_SERVER_PORT:=9090}

# Open Grafana dashboard when the server is up and running
(
  open=$(which open &>/dev/null && echo "open" || echo "xdg-open")
  tries=100
  while ! curl "$GRAFANA_STARTUP_URL" &>/dev/null ; do
    sleep 0.5
    tries=$(( tries - 1 ))
    if [[ $tries == 0 ]] ; then
      exit 1
    fi
  done
  echo "Opening $GRAFANA_STARTUP_URL"
  "$open" "$GRAFANA_STARTUP_URL"
) &

function sync () {
  local json=$(curl "$GRAFANA_DASHBOARD_URL" 2>/dev/null)
  if [[ -z "$json" ]] ; then
    echo "$0: WARNING: Could not download dashboard from $GRAFANA_DASHBOARD_URL"
    return
  fi

  json=$(echo "$json" | jq -M -r '.dashboard | del(.version)')
  current=$(cat "$GRAFANA_DASHBOARD_FILE_PATH" | jq -M -r 'del(.version)')
  # If the dashboard hasn't changed, don't write a new JSON file, to avoid
  # updating the file timestamp (causing Grafana to show "someone else updated
  # this dashboard")
  if [ "$json" == "$current" ] ; then return; fi
  echo "$0: Detected change in Grafana dashboard. Saving to $GRAFANA_DASHBOARD_FILE_PATH"
  echo "$json" > "$GRAFANA_DASHBOARD_FILE_PATH"
}

# Poll for dashboard changes and update the local JSON files.
(
  while true ; do
    sleep 3
    sync
  done
) &

docker_compose_args=("-f" "docker-compose.grafana.yml")
if [[ "$1" == "kube" ]] ; then
  # Start a thread to forward port 9100 locally to the Prometheus server on Kube.
  (
    kubectl --context="$KUBE_CONTEXT" --namespace="$KUBE_NAMESPACE" \
        port-forward "$KUBE_PROM_SERVER_RESOURCE" 9100:"$KUBE_PROM_SERVER_PORT"
  ) &
else
  # Run the Prometheus server locally.
  docker_compose_args+=("-f" "docker-compose.prometheus.yml")
fi

docker-compose "${docker_compose_args[@]}" up
