package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"devshard/types"
)

const (
	rotationRoleRegular = "regular"
	rotationRoleTemp    = "temp"

	defaultEscrowRotationInterval = 15 * time.Second
)

var errDevshardBusy = errors.New("devshard has active requests")

func (g *Gateway) startEscrowRotatorIfEnabled() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.settings.EscrowRotation.Enabled {
		g.startEscrowRotatorLocked()
	}
}

func (g *Gateway) startEscrowRotatorLocked() {
	if g == nil || g.rotatorStop != nil {
		return
	}
	g.rotatorStop = make(chan struct{})
	g.rotatorDone = make(chan struct{})
	go g.runEscrowRotator(g.rotatorStop, g.rotatorDone)
}

func (g *Gateway) stopEscrowRotator() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopEscrowRotatorLocked()
}

func (g *Gateway) stopEscrowRotatorLocked() {
	if g == nil || g.rotatorStop == nil {
		return
	}
	stopCh := g.rotatorStop
	doneCh := g.rotatorDone
	g.rotatorStop = nil
	g.rotatorDone = nil
	close(stopCh)
	g.mu.Unlock()
	<-doneCh
	g.mu.Lock()
}

func (g *Gateway) runEscrowRotator(stopCh <-chan struct{}, doneCh chan<- struct{}) {
	defer close(doneCh)
	g.rotateEscrowsOnce()

	ticker := time.NewTicker(defaultEscrowRotationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			g.rotateEscrowsOnce()
		case <-stopCh:
			return
		}
	}
}

func (g *Gateway) rotateEscrowsOnce() {
	if g == nil || g.phaseGate == nil || g.store == nil {
		return
	}
	g.mu.Lock()
	settings := g.settings
	g.mu.Unlock()
	rotation := settings.EscrowRotation
	if !rotation.Enabled {
		return
	}
	if err := validateGatewaySettings(settings); err != nil {
		log.Printf("escrow_rotation_disabled_invalid_settings error=%v", err)
		return
	}

	snapshot := g.phaseGate.Snapshot()
	if snapshot.EpochIndex == 0 || snapshot.BlockHeight == 0 {
		return
	}
	pocActive, _ := rawPoCBlockingState(snapshot.EpochPhase, snapshot.ConfirmationPoCPhase)
	blocksToPoC := snapshot.pocStartBlockHeight - snapshot.BlockHeight

	if snapshot.EpochPhase == epochPhaseInference && blocksToPoC >= 0 && blocksToPoC <= rotation.PrePoCBlocks {
		g.prepareBridgeEscrows(snapshot, settings)
		return
	}
	if !pocActive {
		g.finishBridgeEscrows(snapshot, settings)
	}
}

func (g *Gateway) prepareBridgeEscrows(snapshot ChainPhaseSnapshot, settings GatewaySettings) {
	epoch := snapshot.EpochIndex
	rotation := settings.EscrowRotation
	if err := g.ensureRotationEscrows(context.Background(), settings, rotationRoleTemp, epoch, rotation.TempCount); err != nil {
		log.Printf("escrow_rotation_temp_create_failed epoch=%d error=%v", epoch, err)
		return
	}
	state, ok, err := g.store.LoadState()
	if err != nil || !ok {
		log.Printf("escrow_rotation_load_state_failed epoch=%d error=%v", epoch, err)
		return
	}
	for _, devshard := range state.Devshards {
		if devshard.RotationRole == rotationRoleTemp {
			continue
		}
		if !devshard.Active {
			continue
		}
		log.Printf("escrow_rotation_settling_regular epoch=%d escrow=%s", epoch, devshard.ID)
		if _, err := g.settleDevshardOnChain(context.Background(), devshard.ID, adminSettleEscrowRequest{}); err != nil {
			log.Printf("escrow_rotation_regular_settle_failed epoch=%d escrow=%s error=%v", epoch, devshard.ID, err)
		}
	}
}

func (g *Gateway) finishBridgeEscrows(snapshot ChainPhaseSnapshot, settings GatewaySettings) {
	epoch := snapshot.EpochIndex
	state, ok, err := g.store.LoadState()
	if err != nil || !ok {
		log.Printf("escrow_rotation_load_state_failed epoch=%d error=%v", epoch, err)
		return
	}
	hasBridgeFromPreviousEpoch := false
	for _, devshard := range state.Devshards {
		if devshard.RotationRole == rotationRoleTemp && devshard.RotationEpoch < epoch && devshard.Active {
			hasBridgeFromPreviousEpoch = true
			break
		}
	}
	if !hasBridgeFromPreviousEpoch {
		return
	}
	if err := g.ensureRotationEscrows(context.Background(), settings, rotationRoleRegular, epoch, settings.EscrowRotation.TargetCount); err != nil {
		log.Printf("escrow_rotation_regular_create_failed epoch=%d error=%v", epoch, err)
		return
	}
	state, ok, err = g.store.LoadState()
	if err != nil || !ok {
		log.Printf("escrow_rotation_reload_state_failed epoch=%d error=%v", epoch, err)
		return
	}
	for _, devshard := range state.Devshards {
		if devshard.RotationRole != rotationRoleTemp || devshard.RotationEpoch >= epoch || !devshard.Active {
			continue
		}
		log.Printf("escrow_rotation_settling_temp epoch=%d temp_epoch=%d escrow=%s", epoch, devshard.RotationEpoch, devshard.ID)
		if _, err := g.settleDevshardOnChain(context.Background(), devshard.ID, adminSettleEscrowRequest{}); err != nil {
			log.Printf("escrow_rotation_temp_settle_failed epoch=%d escrow=%s error=%v", epoch, devshard.ID, err)
		}
	}
}

func (g *Gateway) ensureRotationEscrows(ctx context.Context, settings GatewaySettings, role string, epoch uint64, target int) error {
	if target <= 0 {
		return nil
	}
	state, ok, err := g.store.LoadState()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("gateway state is not initialized")
	}
	count := 0
	for _, devshard := range state.Devshards {
		if devshard.RotationRole == role && devshard.RotationEpoch == epoch && devshard.Active {
			count++
		}
	}
	for count < target {
		if _, err := g.createRotationEscrow(ctx, settings, role, epoch); err != nil {
			return err
		}
		count++
	}
	return nil
}

func (g *Gateway) createRotationEscrow(ctx context.Context, settings GatewaySettings, role string, epoch uint64) (*CreateDevshardEscrowResult, error) {
	rotation := settings.EscrowRotation
	modelID := strings.TrimSpace(rotation.ModelID)
	if modelID == "" {
		modelID = settings.DefaultModel
	}
	signer, _, err := signerFromRequestKey("", rotation.PrivateKeyEnv)
	if err != nil {
		return nil, err
	}
	txClient, err := newGatewayRESTChainTxClient(settings, "", "", 0, 0)
	if err != nil {
		return nil, err
	}
	result, err := txClient.CreateDevshardEscrow(ctx, signer, rotation.Amount, modelID)
	if err != nil {
		return nil, err
	}
	record := GatewayDevshardState{
		RuntimeConfig: RuntimeConfig{
			ID:            strconv.FormatUint(result.EscrowID, 10),
			PrivateKeyEnv: strings.TrimSpace(rotation.PrivateKeyEnv),
			Model:         modelID,
		},
		Active:        true,
		RotationRole:  role,
		RotationEpoch: epoch,
	}
	if err := g.addCreatedEscrowRuntime(record); err != nil {
		return nil, err
	}
	log.Printf("escrow_rotation_created role=%s epoch=%d escrow=%d tx_hash=%s", role, epoch, result.EscrowID, result.TxHash)
	return result, nil
}

func (g *Gateway) settleDevshardOnChain(ctx context.Context, id string, req adminSettleEscrowRequest) (*SettleDevshardEscrowResult, error) {
	log.Printf("devshard_settle_start escrow=%s", id)
	g.mu.Lock()
	rt, ok := g.runtimes[id]
	if ok && rt.activeRequests.Load() > 0 {
		g.mu.Unlock()
		log.Printf("devshard_settle_blocked escrow=%s reason=active_requests count=%d", id, rt.activeRequests.Load())
		return nil, errDevshardBusy
	}
	if ok {
		rt.active.Store(false)
	}
	g.mu.Unlock()
	if !ok {
		log.Printf("devshard_settle_failed escrow=%s stage=runtime_lookup error=%q", id, "devshard is not active")
		return nil, fmt.Errorf("devshard %s is not active", id)
	}
	if err := g.store.SetDevshardActive(id, false); err != nil {
		log.Printf("devshard_settle_failed escrow=%s stage=persist_deactivate error=%q", id, err.Error())
		return nil, err
	}

	privateKey, privateKeyEnv, err := g.resolveDevshardSettlementKey(id, req)
	if err != nil {
		log.Printf("devshard_settle_failed escrow=%s stage=resolve_key error=%q", id, err.Error())
		return nil, err
	}
	signer, _, err := signerFromRequestKey(privateKey, privateKeyEnv)
	if err != nil {
		log.Printf("devshard_settle_failed escrow=%s stage=load_key key_env=%q error=%q", id, privateKeyEnv, err.Error())
		return nil, err
	}
	log.Printf("devshard_settle_key_loaded escrow=%s settler=%s key_env=%q", id, signer.Address(), privateKeyEnv)
	if rt.proxy.sm.Phase() != types.PhaseSettlement {
		g.finalizeMu.Lock()
		log.Printf("gateway_finalize_lock_acquired escrow=%s path=rotation_settle", id)
		if err := rt.session.Finalize(ctx); err != nil {
			g.finalizeMu.Unlock()
			log.Printf("devshard_settle_failed escrow=%s stage=finalize error=%q", id, err.Error())
			return nil, err
		}
		g.finalizeMu.Unlock()
		log.Printf("devshard_settle_finalize_completed escrow=%s phase=%s", id, sessionPhaseLabel(rt.proxy.sm.Phase()))
	} else {
		log.Printf("devshard_settle_finalize_skipped escrow=%s phase=%s", id, sessionPhaseLabel(rt.proxy.sm.Phase()))
	}
	settlement, err := rt.proxy.settlementJSON()
	if err != nil {
		log.Printf("devshard_settle_failed escrow=%s stage=settlement_json error=%q", id, err.Error())
		return nil, err
	}
	g.mu.Lock()
	settings := g.settings
	g.mu.Unlock()
	txClient, err := newGatewayRESTChainTxClient(settings, req.ChainID, req.FeeDenom, req.FeeAmount, req.GasLimit)
	if err != nil {
		log.Printf("devshard_settle_failed escrow=%s stage=tx_client chain_rest=%q error=%q", id, settings.ChainREST, err.Error())
		return nil, err
	}
	log.Printf("devshard_settle_broadcast_start escrow=%s chain_rest=%q chain_id_override=%q gas_limit=%d fee_denom=%q fee_amount=%d",
		id, settings.ChainREST, req.ChainID, req.GasLimit, req.FeeDenom, req.FeeAmount)
	result, err := txClient.SettleDevshardEscrow(ctx, signer, settlement)
	if err != nil {
		log.Printf("devshard_settle_failed escrow=%s stage=broadcast chain_rest=%q error=%q", id, settings.ChainREST, err.Error())
		return nil, err
	}
	log.Printf("devshard_settle_submitted escrow=%s tx_hash=%s settler=%s", id, result.TxHash, result.Settler)
	return result, nil
}
