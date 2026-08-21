#!/usr/bin/env bash
#
# tofu-runner entrypoint
#
# Implements the runner contract documented in internal/controller/runner.go.
# Reads module source, runs tofu init + plan/apply, and writes results back
# to Kubernetes Secrets via kubectl.

set -euo pipefail

# ---------------------------------------------------------------------------
# Environment (set by the controller)
# ---------------------------------------------------------------------------
ACTION="${TOFU_ACTION:?TOFU_ACTION is required (plan|apply)}"
NAMESPACE="${TOFU_NAMESPACE:?TOFU_NAMESPACE is required}"
STATE_SECRET="${TOFU_STATE_SECRET:?TOFU_STATE_SECRET is required}"
PLAN_SECRET="${TOFU_PLAN_SECRET:?TOFU_PLAN_SECRET is required}"
OUTPUTS_SECRET="${TOFU_OUTPUTS_SECRET:?TOFU_OUTPUTS_SECRET is required}"

# Mount paths, overridable for custom mounts or local testing.
TOFU_CONFIG_DIR="${TOFU_CONFIG_DIR:-/tofu/config}"
TFVARS_FILE="${TOFU_TFVARS_FILE:-/tofu/tfvars.json}"
BACKEND_CONFIG_FILE="${TOFU_BACKEND_CONFIG_FILE:-/tofu/backend-config.json}"

WORK_DIR="/tmp/tofu-work"
mkdir -p "$WORK_DIR"
cd "$WORK_DIR"

# ---------------------------------------------------------------------------
# Module source
# ---------------------------------------------------------------------------

prepare_git_source() {
  local url="${TOFU_GIT_URL:?TOFU_GIT_URL is required}"
  local ref="${TOFU_GIT_REF:-}"
  local subpath="${TOFU_GIT_SUBPATH:-}"

  echo "Cloning ${url} ..."
  git clone --depth 1 ${ref:+--branch "$ref"} "$url" repo

  if [[ -n "$subpath" ]]; then
    cd "repo/$subpath"
  else
    cd repo
  fi

  WORK_DIR="$PWD"
}

prepare_configmap_source() {
  local cm_name="${TOFU_CONFIGMAP:?TOFU_CONFIGMAP is required}"
  echo "Copying module files from ConfigMap ${cm_name} ..."

  # Files are mounted at $TOFU_CONFIG_DIR by the controller.
  # We need to extract them to a writable directory.
  cp -a "$TOFU_CONFIG_DIR/." .
}

# Determine module source
if [[ -n "${TOFU_GIT_URL:-}" ]]; then
  prepare_git_source
elif [[ -n "${TOFU_CONFIGMAP:-}" ]]; then
  prepare_configmap_source
else
  echo "ERROR: No module source configured (set TOFU_GIT_URL or TOFU_CONFIGMAP)" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Backend configuration
# ---------------------------------------------------------------------------

BACKEND_ARGS=()
if [[ -n "${TOFU_BACKEND_TYPE:-}" && -f "$BACKEND_CONFIG_FILE" ]]; then
  BACKEND_ARGS=(-backend-config="$BACKEND_CONFIG_FILE")
fi

# ---------------------------------------------------------------------------
# Init
# ---------------------------------------------------------------------------

echo "Running: tofu init ${BACKEND_ARGS[*]:-}"
tofu init "${BACKEND_ARGS[@]+"${BACKEND_ARGS[@]}"}"

# ---------------------------------------------------------------------------
# plan
# ---------------------------------------------------------------------------

run_plan() {
  echo "Running: tofu plan -out=plan.tfplan -var-file=$TFVARS_FILE"
  tofu plan -out=plan.tfplan -var-file="$TFVARS_FILE" -no-color 2>&1 | tee plan.txt

  # Persist plan and human-readable output to the plan Secret.
  kubectl patch secret "$PLAN_SECRET" \
    --namespace "$NAMESPACE" \
    --type merge \
    --patch "$(printf '{"data":{"plan.tfplan":"%s","plan.txt":"%s"}}' \
      "$(base64 < plan.tfplan | tr -d '\n')" \
      "$(base64 < plan.txt | tr -d '\n')")"

  echo "Plan written to Secret ${PLAN_SECRET}"

  # Persist state.
  persist_state
}

# ---------------------------------------------------------------------------
# apply
# ---------------------------------------------------------------------------

run_apply() {
  # Download the saved plan from the plan Secret.
  echo "Downloading plan from Secret ${PLAN_SECRET} ..."
  kubectl get secret "$PLAN_SECRET" \
    --namespace "$NAMESPACE" \
    -o jsonpath='{.data.plan\.tfplan}' | base64 -d > plan.tfplan

  if [[ ! -s plan.tfplan ]]; then
    echo "ERROR: Empty or missing plan.tfplan in Secret ${PLAN_SECRET}" >&2
    exit 1
  fi

  echo "Running: tofu apply plan.tfplan"
  tofu apply -auto-approve plan.tfplan -no-color 2>&1 | tee apply.txt

  # Persist state.
  persist_state

  # Persist outputs.
  persist_outputs
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

persist_state() {
  if [[ -f terraform.tfstate ]]; then
    kubectl patch secret "$STATE_SECRET" \
      --namespace "$NAMESPACE" \
      --type merge \
      --patch "$(printf '{"data":{"terraform.tfstate":"%s"}}' \
        "$(base64 < terraform.tfstate | tr -d '\n')")"
    echo "State written to Secret ${STATE_SECRET}"
  else
    echo "WARNING: No terraform.tfstate produced; skipping state persistence"
  fi
}

persist_outputs() {
  echo "Collecting outputs ..."
  if tofu output -json > outputs.json 2>/dev/null; then
    kubectl patch secret "$OUTPUTS_SECRET" \
      --namespace "$NAMESPACE" \
      --type merge \
      --patch "$(printf '{"data":{"outputs.json":"%s"}}' \
        "$(base64 < outputs.json | tr -d '\n')")"
    echo "Outputs written to Secret ${OUTPUTS_SECRET}"
  else
    echo "WARNING: No outputs or tofu output failed; clearing outputs Secret"
    kubectl patch secret "$OUTPUTS_SECRET" \
      --namespace "$NAMESPACE" \
      --type merge \
      --patch '{"data":{"outputs.json":"e30="}}'
  fi
}

# ---------------------------------------------------------------------------
# Action dispatch
# ---------------------------------------------------------------------------

case "$ACTION" in
  plan)
    run_plan
    ;;
  apply)
    run_apply
    ;;
  *)
    echo "ERROR: Unknown TOFU_ACTION: ${ACTION}" >&2
    exit 1
    ;;
esac

echo "Done (${ACTION})"
