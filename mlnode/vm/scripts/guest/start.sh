#!/bin/bash
# Start Confidential MLNode: vLLM + TEE-enabled API.
# Run as root inside the SEV-SNP VM after guest/setup.sh.
#
# Usage:
#   sudo bash guest/start.sh              # start
#   sudo bash guest/start.sh stop         # stop
#   sudo bash guest/start.sh status       # check
#   sudo bash guest/start.sh test         # start + E2E test

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
log()  { echo -e "${GREEN}[+]${NC} $*"; }
warn() { echo -e "${YELLOW}[!]${NC} $*"; }
err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die()  { err "$1"; [ -n "${2:-}" ] && echo -e "${YELLOW}[FIX]${NC} $2" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Must run as root (sudo)"

export PYTHONPATH="/app/packages/api/src:/app/packages/pow/src:/app/packages/train/src:/app/packages/common/src"
export VLLM_TARGET_DEVICE=cpu
export VLLM_ENABLE_V1_MULTIPROCESSING=0
export VLLM_ATTENTION_BACKEND=TORCH_SDPA
export TEE_ENABLED=1
export SNPGUEST_PATH=/usr/local/bin/snpguest

VLLM_PORT=5000
MLNODE_PORT=8080
MODEL="Qwen/Qwen2.5-0.5B-Instruct"

vllm_running()   { curl -s "http://127.0.0.1:$VLLM_PORT/health" > /dev/null 2>&1; }
mlnode_running()  { curl -s "http://127.0.0.1:$MLNODE_PORT/health" > /dev/null 2>&1; }

stop_all() {
    log "Stopping..."
    pkill -f "vllm.entrypoints" 2>/dev/null || true
    pkill -f "uvicorn.*api.app" 2>/dev/null || true
    sleep 2
    log "Stopped"
}

show_status() {
    echo "vLLM:   $(vllm_running && echo 'RUNNING' || echo 'STOPPED')"
    echo "MLNode: $(mlnode_running && echo 'RUNNING' || echo 'STOPPED')"
    if mlnode_running; then
        local tee_type
        tee_type=$(curl -s "http://127.0.0.1:$MLNODE_PORT/attestation" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('tee_type','unknown'))" 2>/dev/null || echo "unknown")
        echo "TEE:    $tee_type"
    fi
}

start_vllm() {
    if vllm_running; then
        log "vLLM already running"
        return 0
    fi

    log "Starting vLLM ($MODEL, CPU)..."
    # shellcheck disable=SC1091  # venv created at runtime by guest/setup.sh
    source /opt/mlnode/bin/activate
    nohup python3 -m vllm.entrypoints.openai.api_server \
        --model "$MODEL" \
        --dtype float32 \
        --max-model-len 512 \
        --enforce-eager \
        --host 127.0.0.1 \
        --port "$VLLM_PORT" \
        > /tmp/vllm.log 2>&1 &

    log "Waiting for vLLM to load model (CPU mode, takes 1-5 min)..."
    for i in $(seq 1 120); do
        if vllm_running; then
            log "vLLM ready (${i}x5s)"
            return 0
        fi
        # Check if process died
        if ! pgrep -f "vllm.entrypoints" > /dev/null 2>&1; then
            err "vLLM process died. Last log:"
            tail -15 /tmp/vllm.log >&2
            die "vLLM failed to start" "Check /tmp/vllm.log for details"
        fi
        sleep 5
    done
    die "vLLM timeout (10 min)" "Check /tmp/vllm.log"
}

start_mlnode() {
    if mlnode_running; then
        log "MLNode already running"
        return 0
    fi

    log "Starting MLNode API (TEE_ENABLED=$TEE_ENABLED)..."
    # shellcheck disable=SC1091  # venv created at runtime by guest/setup.sh
    source /opt/mlnode/bin/activate
    cd /app/packages/api/src || die "cannot cd /app/packages/api/src"
    nohup python3 -m uvicorn api.app:app \
        --host 0.0.0.0 \
        --port "$MLNODE_PORT" \
        --log-level warning \
        > /tmp/mlnode.log 2>&1 &

    sleep 15

    if mlnode_running; then
        log "MLNode ready on port $MLNODE_PORT"
        local tee_type
        tee_type=$(curl -s "http://127.0.0.1:$MLNODE_PORT/attestation" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('tee_type','unknown'))" 2>/dev/null || echo "unknown")
        log "TEE attestation: $tee_type"
    else
        err "MLNode failed. Last log:"
        tail -20 /tmp/mlnode.log >&2
        die "MLNode failed to start" "Check /tmp/mlnode.log"
    fi
}

run_e2e_test() {
    log "Running E2E encrypted inference test..."
    # shellcheck disable=SC1091  # venv created at runtime by guest/setup.sh
    source /opt/mlnode/bin/activate
    python3 /root/gonka/mlnode/packages/api/client/tee_client.py \
        --url "http://127.0.0.1:$MLNODE_PORT" \
        --prompt "What is confidential computing?"
}

case "${1:-start}" in
    stop)
        stop_all
        ;;
    status)
        show_status
        ;;
    test)
        start_vllm
        start_mlnode
        run_e2e_test
        ;;
    start|"")
        start_vllm
        start_mlnode
        echo ""
        show_status
        echo ""
        log "To test: sudo bash guest/start.sh test"
        ;;
    *)
        echo "Usage: sudo bash guest/start.sh [start|stop|status|test]"
        exit 1
        ;;
esac
