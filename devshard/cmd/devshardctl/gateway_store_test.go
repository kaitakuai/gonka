package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayStoreInitializeAndLoadState(t *testing.T) {
	store, err := NewGatewayStore(filepath.Join(t.TempDir(), "gateway.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	settings := GatewaySettings{
		ChainREST:               "http://node:1317",
		PublicAPI:               "http://api:9000",
		DefaultModel:            "Qwen/Test",
		DefaultRequestMaxTokens: 1234,
		MaxConcurrentRequests:   5,
		MaxInputTokensInFlight:  999,
	}.WithTuningDefaults()
	devshards := []GatewayDevshardState{{
		RuntimeConfig: RuntimeConfig{
			ID:            "12",
			PrivateKeyHex: "secret",
			Model:         "Qwen/Test",
			StoragePath:   "/root/.devshardctl/escrow-12/state.db",
		},
		Active:        true,
		RotationRole:  rotationRoleRegular,
		RotationEpoch: 7,
	}}

	require.NoError(t, store.Initialize(settings, devshards))

	state, ok, err := store.LoadState()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, settings, state.Settings)
	require.Len(t, state.Devshards, 1)
	require.Equal(t, "12", state.Devshards[0].ID)
	require.True(t, state.Devshards[0].Active)
	require.Equal(t, "/root/.devshardctl/escrow-12/state.db", state.Devshards[0].StoragePath)
	require.Equal(t, rotationRoleRegular, state.Devshards[0].RotationRole)
	require.EqualValues(t, 7, state.Devshards[0].RotationEpoch)
	require.EqualValues(t, defaultEscrowRotationAmount, state.Settings.EscrowRotation.Amount)
	require.Equal(t, "Qwen/Test", state.Settings.EscrowRotation.ModelID)
	require.False(t, state.Settings.Disabled.Enabled)
	require.Equal(t, defaultGatewayDisabledMessage, state.Settings.Disabled.Message)
}

func TestAdminAuthMiddlewareRequiresAdminKey(t *testing.T) {
	for _, path := range []string{
		"/v1/admin/state",
		"/v1/finalize",
		"/devshard/12/v1/finalize",
		"/v1/state",
		"/devshard/12/v1/state",
		"/v1/debug/state",
		"/devshard/12/v1/debug/signatures/collect",
	} {
		handler := adminAuthMiddleware("adminkey", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)

		req = httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer adminkey")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNoContent, rec.Code)
	}
}

func TestAdminFinalizeBypassesPublicAPIKeyAuth(t *testing.T) {
	handler := adminAuthMiddleware("adminkey", bearerAuthMiddleware(map[string]struct{}{"publickey": {}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	req := httptest.NewRequest(http.MethodPost, "/v1/finalize", nil)
	req.Header.Set("Authorization", "Bearer adminkey")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestGatewayStoreUpdateSettings(t *testing.T) {
	store, err := NewGatewayStore(filepath.Join(t.TempDir(), "gateway.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	require.NoError(t, store.Initialize(GatewaySettings{
		ChainREST:               "http://node:1317",
		PublicAPI:               "http://api:9000",
		DefaultModel:            "Qwen/Test",
		DefaultRequestMaxTokens: 1000,
		MaxConcurrentRequests:   2,
		MaxInputTokensInFlight:  200,
	}, nil))

	require.NoError(t, store.UpdateSettings(GatewaySettings{
		ChainREST:               "http://node:1317",
		PublicAPI:               "http://api:9000",
		DefaultModel:            "Qwen/Test",
		DefaultRequestMaxTokens: 2000,
		MaxConcurrentRequests:   5,
		MaxInputTokensInFlight:  500,
		Disabled: GatewayDisabledSettings{
			Enabled: true,
			Message: "please use https://node4.gonka.ai/v1/ base url",
		},
		ParticipantThrottle: ParticipantThrottleSettings{
			RequestBurst:                   42,
			RecoveryPerMinute:              7,
			HTTPQuarantineMS:               1100,
			TransportFailureQuarantineMS:   1200,
			EmptyStreamQuarantineMS:        1300,
			StalledWinnerQuarantineMS:      1400,
			EmptyStreamQuarantineThreshold: 2,
		},
		Redundancy: RedundancySettings{
			ReceiptTimeoutMS:             1500,
			FirstTokenTimeoutFloorMS:     1600,
			PerInputTokenFirstTokenLagMS: 17,
			InterChunkStallTimeoutMS:     1800,
			NonStreamResponseFloorMS:     1900,
			PerInputTokenResponseLagMS:   20,
			SecondaryWaitAfterWinnerMS:   2100,
			ParallelAdvantageThreshold:   0.4,
			UnresponsiveThreshold:        0.8,
		},
		EscrowRotation: EscrowRotationSettings{
			Enabled:       true,
			PrePoCBlocks:  123,
			TempCount:     4,
			TargetCount:   12,
			Amount:        999,
			ModelID:       "Qwen/Rotate",
			PrivateKeyEnv: "DEVSHARD_ROTATION_KEY",
		},
	}))

	state, ok, err := store.LoadState()
	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, 2000, state.Settings.DefaultRequestMaxTokens)
	require.EqualValues(t, 5, state.Settings.MaxConcurrentRequests)
	require.EqualValues(t, 500, state.Settings.MaxInputTokensInFlight)
	require.True(t, state.Settings.Disabled.Enabled)
	require.Equal(t, "please use https://node4.gonka.ai/v1/ base url", state.Settings.Disabled.Message)
	require.EqualValues(t, 42, state.Settings.ParticipantThrottle.RequestBurst)
	require.EqualValues(t, 1200, state.Settings.ParticipantThrottle.TransportFailureQuarantineMS)
	require.EqualValues(t, 2, state.Settings.ParticipantThrottle.EmptyStreamQuarantineThreshold)
	require.EqualValues(t, 1500, state.Settings.Redundancy.ReceiptTimeoutMS)
	require.EqualValues(t, 17, state.Settings.Redundancy.PerInputTokenFirstTokenLagMS)
	require.Equal(t, 0.4, state.Settings.Redundancy.ParallelAdvantageThreshold)
	require.True(t, state.Settings.EscrowRotation.Enabled)
	require.EqualValues(t, 123, state.Settings.EscrowRotation.PrePoCBlocks)
	require.EqualValues(t, 4, state.Settings.EscrowRotation.TempCount)
	require.EqualValues(t, 12, state.Settings.EscrowRotation.TargetCount)
	require.EqualValues(t, 999, state.Settings.EscrowRotation.Amount)
	require.Equal(t, "Qwen/Rotate", state.Settings.EscrowRotation.ModelID)
	require.Equal(t, "DEVSHARD_ROTATION_KEY", state.Settings.EscrowRotation.PrivateKeyEnv)
}

func TestValidateGatewaySettingsRequiresRotationFunding(t *testing.T) {
	settings := GatewaySettings{
		ChainREST:               "http://node:1317",
		PublicAPI:               "http://api:9000",
		DefaultModel:            "Qwen/Test",
		DefaultRequestMaxTokens: 1000,
		MaxConcurrentRequests:   2,
	}.WithTuningDefaults()
	settings.EscrowRotation.Enabled = true
	settings.EscrowRotation.Amount = 0

	err := validateGatewaySettings(settings)
	require.Error(t, err)
	require.Contains(t, err.Error(), "amount")

	settings.EscrowRotation.Amount = 1000
	err = validateGatewaySettings(settings)
	require.Error(t, err)
	require.Contains(t, err.Error(), "private_key_env")

	settings.EscrowRotation.PrivateKeyEnv = "DEVSHARD_PRIVATE_KEY"
	require.NoError(t, validateGatewaySettings(settings))
}
