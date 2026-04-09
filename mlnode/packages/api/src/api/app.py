import asyncio
import os

from fastapi import FastAPI, Depends
from contextlib import asynccontextmanager

from api.inference.manager import InferenceManager
from api.inference.routes import router as inference_router
from api.inference.pow_v2_routes import router as pow_v2_router

from api.models.manager import ModelManager
from api.models.routes import router as models_router

from api.gpu.manager import GPUManager
from api.gpu.routes import router as gpu_router

from zeroband.service.manager import TrainManager
from zeroband.service.routes import router as train_router

from pow.service.manager import PowManager
from pow.service.routes import router as pow_router

from api.health import router as health_router

from api.service_management import (
    ServiceState,
    check_service_conflicts,
    API_PREFIX
)
from api.routes import router as api_router
from api.watcher import watch_managers
from api.proxy import ProxyMiddleware, start_vllm_proxy, stop_vllm_proxy, setup_vllm_proxy, start_backward_compatibility, stop_backward_compatibility

# TEE support (Confidential MLNode — proposal #951)
TEE_ENABLED = os.getenv("TEE_ENABLED", "0") == "1"
if TEE_ENABLED:
    from fastapi import HTTPException as _HTTPException
    from api.tee.routes import router as tee_router

    # Spec §10.2 MI-4: runtime model change MUST be blocked in TEE mode
    _TEE_BLOCKED_PREFIXES = ("/inference/up", "/inference/down", "/models/download")

    async def _tee_block_model_change(request):
        for prefix in _TEE_BLOCKED_PREFIXES:
            if request.url.path.endswith(prefix):
                raise _HTTPException(
                    status_code=403,
                    detail="Model changes are not allowed in TEE mode (spec §10.2 MI-4)",
                )

WATCH_INTERVAL = 2


@asynccontextmanager
async def lifespan(app: FastAPI):
    app.state.service_state = ServiceState.STOPPED
    app.state.pow_manager = PowManager()
    app.state.inference_manager = InferenceManager()
    app.state.train_manager = TrainManager()
    app.state.model_manager = ModelManager()
    app.state.gpu_manager = GPUManager()

    # --- TEE initialization (proposal #951) ---
    if TEE_ENABLED:
        from api.tee.detect import detect_tee
        from api.tee.crypto import TEEKeyManager
        from api.tee.attestation import generate_attestation
        from common.logger import create_logger
        tee_logger = create_logger("tee")
        tee_logger.info("TEE mode enabled — detecting environment")

        # Step 1: Detect TEE hardware (exits if no CPU TEE found)
        tee_info = detect_tee()
        app.state.tee_info = tee_info

        # Step 2: Generate keys (only after TEE confirmed)
        keys = TEEKeyManager()
        image_hash = os.getenv("TEE_IMAGE_HASH", None)
        attestation = generate_attestation(keys, tee_info, image_hash=image_hash)
        app.state.tee_keys = keys
        app.state.tee_attestation = attestation
        tee_logger.info("TEE attestation ready")

    await start_vllm_proxy()

    monitor_task = asyncio.create_task(
        watch_managers(
            app,
            [
                app.state.pow_manager,
                app.state.inference_manager,
                app.state.train_manager,
            ],
            interval=WATCH_INTERVAL
        )
    )

    yield
    
    if app.state.pow_manager.is_running():
        app.state.pow_manager.stop()
    if app.state.inference_manager.is_running():
        # Use async stop in async context to avoid blocking event loop
        await app.state.inference_manager._async_stop()
    if app.state.train_manager.is_running():
        app.state.train_manager.stop()

    app.state.gpu_manager._shutdown_nvml()

    # Close TEE httpx client
    if TEE_ENABLED:
        from api.tee.routes import close_vllm_client
        await close_vllm_client()

    await stop_vllm_proxy()
    await stop_backward_compatibility()

    monitor_task.cancel()
    try:
        await monitor_task
    except asyncio.CancelledError:
        pass


app = FastAPI(lifespan=lifespan)

app.include_router(health_router)

# TEE mode: disable plain /v1 proxy — all inference must be encrypted
if not TEE_ENABLED:
    app.add_middleware(ProxyMiddleware)

app.include_router(
    pow_router,
    prefix=API_PREFIX,
    tags=["PoW"],
    dependencies=[Depends(check_service_conflicts)]
)

app.include_router(
    train_router,
    prefix=API_PREFIX,
    tags=["Train"],
    dependencies=[Depends(check_service_conflicts)]
)

_inference_deps = [Depends(check_service_conflicts)]
if TEE_ENABLED:
    _inference_deps.append(Depends(_tee_block_model_change))
app.include_router(
    inference_router,
    prefix=API_PREFIX,
    tags=["Inference"],
    dependencies=_inference_deps
)

# PoC v2 routes work when inference (vLLM) is running - no conflict check needed
app.include_router(
    pow_v2_router,
    prefix=API_PREFIX,
    tags=["PoC v2"],
)

app.include_router(
    api_router,
    prefix=API_PREFIX,
    tags=["API"],
)

# TEE routes (proposal #951)
if TEE_ENABLED:
    app.include_router(
        tee_router,
        tags=["TEE"],
    )

_models_deps = []
if TEE_ENABLED:
    _models_deps.append(Depends(_tee_block_model_change))
app.include_router(
    models_router,
    prefix=API_PREFIX + "/models",
    tags=["Models"],
    dependencies=_models_deps if _models_deps else None,
)

app.include_router(
    gpu_router,
    prefix=API_PREFIX + "/gpu",
    tags=["GPU"],
)
