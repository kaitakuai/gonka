package keeper

import (
	"context"
	"fmt"
	"math"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
)

func (k Keeper) GetDevshardHostEpochStats(ctx context.Context, epochIndex uint64, participant sdk.AccAddress) (types.DevshardHostEpochStats, bool) {
	v, err := k.DevshardHostEpochStatsMap.Get(ctx, collections.Join(epochIndex, participant))
	if err != nil {
		return types.DevshardHostEpochStats{}, false
	}
	return v, true
}

func (k Keeper) AggregateDevshardHostStats(ctx context.Context, epochIndex uint64, participant sdk.AccAddress, slotStats types.DevshardSettlementHostStats) error {
	key := collections.Join(epochIndex, participant)
	existing, err := k.DevshardHostEpochStatsMap.Get(ctx, key)
	if err != nil {
		existing = types.DevshardHostEpochStats{
			Participant: participant.String(),
			EpochIndex:  epochIndex,
		}
	}
	existing.Missed += slotStats.Missed
	existing.Invalid += slotStats.Invalid
	if existing.Cost > math.MaxUint64-slotStats.Cost {
		return fmt.Errorf("cost overflow aggregating devshard host stats")
	}
	existing.Cost += slotStats.Cost
	existing.RequiredValidations += slotStats.RequiredValidations
	existing.CompletedValidations += slotStats.CompletedValidations
	return k.DevshardHostEpochStatsMap.Set(ctx, key, existing)
}

func (k Keeper) IncrementDevshardHostEscrowCount(ctx context.Context, epochIndex uint64, participant sdk.AccAddress) error {
	key := collections.Join(epochIndex, participant)
	existing, err := k.DevshardHostEpochStatsMap.Get(ctx, key)
	if err != nil {
		existing = types.DevshardHostEpochStats{
			Participant: participant.String(),
			EpochIndex:  epochIndex,
		}
	}
	existing.EscrowCount++
	return k.DevshardHostEpochStatsMap.Set(ctx, key, existing)
}
