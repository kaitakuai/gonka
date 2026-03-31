#!/bin/bash
# MLNode onstart.sh — V1 engine (vLLM 0.15.1, sm_120 RTX PRO 6000 Blackwell)
# Adapted from production sm120 image for V1 engine.
# Timezone set via /etc/environment (stage3), not here.
set -e

echo "=========================================="
echo "MLNode onstart.sh - $(date)"
echo "=========================================="

# ============================================================================
# IDEMPOTENCY: Kill any existing instances before starting
# ============================================================================
echo "Cleaning up existing processes..."

# Clear Tailscale state for fresh identity on each boot
rm -rf /var/lib/tailscale/*
echo "  ✓ Cleared Tailscale state"

# Kill existing uvicorn (MLNode API)
pkill -9 -f "uvicorn api.app:app" 2>/dev/null || true

# Kill existing vLLM
pkill -9 -f "vllm.entrypoints" 2>/dev/null || true
pkill -9 -f "VLLM::Worker" 2>/dev/null || true

# Kill nginx (will restart below)
pkill -9 -f nginx 2>/dev/null || true

# Kill all python processes (WARNING: aggressive)
pkill -9 -f "python" 2>/dev/null || true

# Wait for ports to be released
sleep 2

# ============================================================================
# TUN DEVICE (for Tailscale)
# ============================================================================
if [ ! -e /dev/net/tun ]; then
    echo "Creating TUN device..."
    mkdir -p /dev/net
    mknod /dev/net/tun c 10 200 2>/dev/null || true
    chmod 600 /dev/net/tun 2>/dev/null || true
fi

# ============================================================================
# CONDA ENVIRONMENT
# ============================================================================
source /root/miniconda3/etc/profile.d/conda.sh
conda activate base

# ============================================================================
# ENVIRONMENT VARIABLES
# ============================================================================
export PYTHONPATH="/app:/app/packages/api/src:/app/packages/pow/src:/app/packages/train/src:/app/packages/common/src"
export HF_HOME=/root/.cache/huggingface
export HF_HUB_ENABLE_HF_TRANSFER=1
export VLLM_ATTENTION_BACKEND=FLASHINFER
export VLLM_USE_V1=1
export VLLM_CUDART_SO_PATH=/usr/local/cuda/targets/x86_64-linux/lib/libcudart.so
export VLLM_GUIDED_DECODING_BACKEND=outlines
export VLLM_ALLOW_INSECURE_SERIALIZATION=1  # Required for PoC v2 with tensor parallelism (TP>1)
export INFERENCE_PORT=5009  # Fixed: avoid conflict with nginx port 5002
export HF_HUB_OFFLINE=1
export TRANSFORMERS_OFFLINE=1
export LD_LIBRARY_PATH="/root/miniconda3/lib/python3.12/site-packages/torch/lib:${LD_LIBRARY_PATH:-}"

# ============================================================================
# MODEL PATH & LOCAL COPY (for faster loading)
# ============================================================================
# Model configuration - change these to switch models (single source of truth)
MODEL_HF_ORG="Qwen"                                    # HuggingFace org (Qwen, gpt-omni, etc.)
MODEL_NAME_SHORT="Qwen3-235B-A22B-Instruct-2507-FP8"   # Model name without org prefix
export MODEL_HF_ORG MODEL_NAME_SHORT

SHARED_MODEL_DIR="/root/autodl-fs/${MODEL_HF_ORG}"
LOCAL_MODEL_DIR="/root/autodl-tmp/models"
export SHARED_MODEL_DIR LOCAL_MODEL_DIR

SHARED_MODEL_PATH="${SHARED_MODEL_DIR}/${MODEL_NAME_SHORT}"
LOCAL_MODEL_PATH="${LOCAL_MODEL_DIR}/${MODEL_NAME_SHORT}"

# Copy model from shared storage to local NVMe for 10x faster loading
copy_model_to_local() {
    echo "=========================================="
    echo "COPYING MODEL TO LOCAL NVMe (parallel rsync)"
    echo "Source: $SHARED_MODEL_PATH"
    echo "Dest:   $LOCAL_MODEL_PATH"
    echo "=========================================="
    
    mkdir -p "$LOCAL_MODEL_DIR"
    
    # Get sizes
    SHARED_SIZE=$(du -sb "$SHARED_MODEL_PATH" | cut -f1)
    TOTAL_FILES=$(find "$SHARED_MODEL_PATH" -type f | wc -l)
    echo "Model size: $(numfmt --to=iec-i --suffix=B $SHARED_SIZE) ($TOTAL_FILES files)"
    
    # Check available space
    AVAIL_SPACE=$(df -B1 /root/autodl-tmp | tail -1 | awk '{print $4}')
    if [ "$SHARED_SIZE" -gt "$AVAIL_SPACE" ]; then
        echo "ERROR: Not enough space! Need $(numfmt --to=iec-i --suffix=B $SHARED_SIZE), have $(numfmt --to=iec-i --suffix=B $AVAIL_SPACE)"
        return 1
    fi
    
    # Parallel rsync copy
    PARALLEL_JOBS=8
    START_TIME=$(date +%s)
    
    echo "[$(date '+%H:%M:%S')] Starting parallel copy with $PARALLEL_JOBS threads..."
    
    # Create destination directory structure first
    mkdir -p "$LOCAL_MODEL_PATH"
    
    # Copy small files (<100MB) with single rsync for efficiency
    echo "[$(date '+%H:%M:%S')] Copying small files..."
    rsync -a --info=progress2 --exclude='*.safetensors' "$SHARED_MODEL_PATH/" "$LOCAL_MODEL_PATH/"
    
    # Copy large files (safetensors) in parallel
    SHARD_COUNT=$(find "$SHARED_MODEL_PATH" -name "*.safetensors" | wc -l)
    if [ "$SHARD_COUNT" -gt 0 ]; then
        echo "[$(date '+%H:%M:%S')] Copying $SHARD_COUNT model shards in parallel..."
        
        find "$SHARED_MODEL_PATH" -name "*.safetensors" -printf "%f\n" | \
        xargs -P $PARALLEL_JOBS -I {} sh -c '
            FILE="{}"
            SRC="'"$SHARED_MODEL_PATH"'/$FILE"
            DST="'"$LOCAL_MODEL_PATH"'/$FILE"
            SIZE=$(stat -c%s "$SRC" 2>/dev/null || echo 0)
            SIZE_HR=$(numfmt --to=iec-i --suffix=B $SIZE 2>/dev/null || echo "$SIZE")
            echo "[$(date +%H:%M:%S)] Copying $FILE ($SIZE_HR)..."
            rsync -a "$SRC" "$DST"
            echo "[$(date +%H:%M:%S)] Done: $FILE"
        '
    fi
    
    END_TIME=$(date +%s)
    ELAPSED=$((END_TIME - START_TIME))
    [ "$ELAPSED" -eq 0 ] && ELAPSED=1
    
    # Verify copy
    echo "[$(date '+%H:%M:%S')] Verifying copy..."
    LOCAL_SIZE=$(du -sb "$LOCAL_MODEL_PATH" | cut -f1)
    LOCAL_FILES=$(find "$LOCAL_MODEL_PATH" -type f | wc -l)
    
    if [ "$LOCAL_FILES" -eq "$TOTAL_FILES" ]; then
        SPEED=$((SHARED_SIZE / ELAPSED / 1024 / 1024))
        echo "=========================================="
        echo "✅ Copy complete!"
        echo "   Time: ${ELAPSED}s"
        echo "   Speed: ~${SPEED} MB/s"
        echo "   Files: $LOCAL_FILES"
        echo "=========================================="
        return 0
    else
        echo "ERROR: Copy verification failed! ($LOCAL_FILES/$TOTAL_FILES files)"
        rm -rf "$LOCAL_MODEL_PATH"
        return 1
    fi
}

# Determine which model path to use
if [ -d "$LOCAL_MODEL_PATH" ] && [ -f "$LOCAL_MODEL_PATH/config.json" ]; then
    # Local copy exists - verify file count
    LOCAL_FILES=$(find "$LOCAL_MODEL_PATH" -name "*.safetensors" 2>/dev/null | wc -l)
    SHARED_FILES=$(find "$SHARED_MODEL_PATH" -name "*.safetensors" 2>/dev/null | wc -l)
    
    if [ "$LOCAL_FILES" -eq "$SHARED_FILES" ] && [ "$LOCAL_FILES" -gt 0 ]; then
        export MODEL_NAME="$LOCAL_MODEL_PATH"
        echo "✅ Using LOCAL NVMe model: $MODEL_NAME ($LOCAL_FILES shards)"
    else
        echo "⚠️ Local model incomplete ($LOCAL_FILES vs $SHARED_FILES shards), re-copying..."
        rm -rf "$LOCAL_MODEL_PATH"
        if copy_model_to_local; then
            export MODEL_NAME="$LOCAL_MODEL_PATH"
        else
            export MODEL_NAME="$SHARED_MODEL_PATH"
            echo "⚠️ Falling back to SHARED STORAGE: $MODEL_NAME"
        fi
    fi
elif [ -d "$SHARED_MODEL_PATH" ] && [ -f "$SHARED_MODEL_PATH/config.json" ]; then
    # No local copy - try to copy from shared storage
    if copy_model_to_local; then
        export MODEL_NAME="$LOCAL_MODEL_PATH"
        echo "✅ Using freshly copied LOCAL model: $MODEL_NAME"
    else
        export MODEL_NAME="$SHARED_MODEL_PATH"
        echo "⚠️ Falling back to SHARED STORAGE: $MODEL_NAME"
    fi
else
    echo "ERROR: Model not found in shared storage: $SHARED_MODEL_PATH"
    exit 1
fi

# ============================================================================
# PYTHON SYMLINK
# ============================================================================
if [ ! -f /usr/bin/python3.12 ]; then
    ln -sf /root/miniconda3/bin/python3.12 /usr/bin/python3.12
fi

# ============================================================================
# DISABLE JUPYTER/TENSORBOARD (security)
# ============================================================================
pkill -9 jupyter 2>/dev/null || true
pkill -9 tensorboard 2>/dev/null || true

# ============================================================================
# LOG DIRECTORIES
# ============================================================================
mkdir -p /var/log/monitoring /var/lib/tailscale /var/lib/node_exporter/textfile_collector
touch /var/log/vllm.log /var/log/api.log

# ============================================================================
# SHARED STORAGE & MODEL SETUP
# ============================================================================
if [ ! -d "/root/autodl-fs" ]; then
    echo "ERROR: Shared storage not available!"
    exit 1
fi

REGISTRY_FILE="/root/autodl-fs/node_registry.json"
if [ ! -f "$REGISTRY_FILE" ]; then
    echo "ERROR: Registry file not found: $REGISTRY_FILE"
    exit 1
fi

# ============================================================================
# NODE IDENTITY
# ============================================================================
echo "Verifying node identity..."
python3 /opt/ml_node/claim_node.py

NODE_HOSTNAME=$(python3 -c "import json; print(json.load(open('/root/autodl-tmp/.node_identity'))['node_id'])")
ASSIGNED_IP=$(python3 -c "import json; print(json.load(open('/root/autodl-tmp/.node_identity'))['tailscale_ip'])")
echo "NODE: $NODE_HOSTNAME, IP: $ASSIGNED_IP"

# Write node info metrics for Prometheus
/opt/ml_node/write_node_info.sh

# ============================================================================
# TAILSCALE (idempotent - checks if already running)
# ============================================================================
TAILSCALE_AUTH_KEY="${TAILSCALE_AUTH_KEY:-tskey-auth-kHz7inPPsY11CNTRL-EMV5B4DC95AXkq41D2ET4Aa9SBCUg6RRM}"

if ! pgrep -x tailscaled > /dev/null; then
    echo "Starting Tailscale daemon..."
    tailscaled --tun=userspace-networking --socks5-server=localhost:1055 --outbound-http-proxy-listen=localhost:1080 \
        --state=/var/lib/tailscale/tailscaled.state \
        --socket=/var/run/tailscale/tailscaled.sock \
        >> /var/log/tailscale.log 2>&1 &
    sleep 3
fi

# Clean up old/conflicting Tailscale devices BEFORE connecting
echo "Cleaning up old Tailscale devices..."
python3 /opt/ml_node/fix_tailscale_ip.py --cleanup-only 2>/dev/null || true

tailscale up --authkey="$TAILSCALE_AUTH_KEY" --hostname="$NODE_HOSTNAME" 2>/dev/null || true
sleep 2
TAILSCALE_IP=$(tailscale ip -4 2>/dev/null || echo "connecting...")
echo "Tailscale IP: $TAILSCALE_IP"

# Wait for correct IP before proceeding (max 60 seconds)
MAX_IP_WAIT=60
IP_WAIT_INTERVAL=5
IP_WAITED=0

while [ -n "$ASSIGNED_IP" ] && [ "$TAILSCALE_IP" != "$ASSIGNED_IP" ] && [ "$IP_WAITED" -lt "$MAX_IP_WAIT" ]; do
    echo "⚠️ IP mismatch: got $TAILSCALE_IP, expected $ASSIGNED_IP"
    
    # Try to fix IP
    python3 /opt/ml_node/fix_tailscale_ip.py || true
    
    sleep $IP_WAIT_INTERVAL
    IP_WAITED=$((IP_WAITED + IP_WAIT_INTERVAL))
    
    TAILSCALE_IP=$(tailscale ip -4 2>/dev/null || echo "connecting...")
    echo "Tailscale IP after ${IP_WAITED}s: $TAILSCALE_IP"
done

if [ "$TAILSCALE_IP" != "$ASSIGNED_IP" ]; then
    echo "⚠️ WARNING: Could not get assigned IP after ${MAX_IP_WAIT}s!"
    echo "   Current: $TAILSCALE_IP, Expected: $ASSIGNED_IP"
    echo "   Continuing anyway - network node may not reach us initially"
else
    echo "✓ Tailscale IP correct: $TAILSCALE_IP"
fi

tailscale serve reset 2>/dev/null || true
service cron start 2>/dev/null || true

# ============================================================================
# NGINX (idempotent - kill first, then start)
# ============================================================================
echo "Starting nginx..."
nginx 2>/dev/null || true

# ============================================================================
# MONITORING (idempotent - kill first, then start)
# ============================================================================
echo "Starting monitoring..."
pkill -f node_exporter 2>/dev/null || true
pkill -f nvidia_gpu_exporter 2>/dev/null || true
sleep 1

/opt/monitoring/node_exporter \
    --web.listen-address=0.0.0.0:9102 \
    --collector.textfile.directory=/var/lib/node_exporter/textfile_collector \
    >> /var/log/monitoring/node_exporter.log 2>&1 &

/opt/monitoring/nvidia_gpu_exporter --web.listen-address=0.0.0.0:9835 \
    >> /var/log/monitoring/gpu_exporter.log 2>&1 &

# ============================================================================
# MLNODE API (single run - watchdog handles restarts)
# ============================================================================
if [ ! -d "/app/packages/api/src" ]; then
    echo "ERROR: /app/packages/api/src not found!"
    exit 1
fi

cd /app/packages/api/src

echo "=========================================="
echo "Starting MLNode API"
echo "Model: $MODEL_NAME"
echo "Access: http://$TAILSCALE_IP:8081"
echo "=========================================="

# Run uvicorn - exits when it crashes, watchdog will restart whole script
exec python -m uvicorn api.app:app --host 127.0.0.1 --port 8080 2>&1 | tee -a /var/log/api.log
