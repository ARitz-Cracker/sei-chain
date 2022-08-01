package dex

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/CosmWasm/wasmd/x/wasm"
	"github.com/armon/go-metrics"
	"github.com/gorilla/mux"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"

	abci "github.com/tendermint/tendermint/abci/types"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	cdctypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/telemetry"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/sei-protocol/sei-chain/utils/datastructures"
	"github.com/sei-protocol/sei-chain/utils/tracing"
	"github.com/sei-protocol/sei-chain/x/dex/client/cli/query"
	"github.com/sei-protocol/sei-chain/x/dex/client/cli/tx"
	"github.com/sei-protocol/sei-chain/x/dex/contract"
	"github.com/sei-protocol/sei-chain/x/dex/keeper"
	dexkeeperabci "github.com/sei-protocol/sei-chain/x/dex/keeper/abci"
	"github.com/sei-protocol/sei-chain/x/dex/keeper/msgserver"
	dexkeeperquery "github.com/sei-protocol/sei-chain/x/dex/keeper/query"
	dexkeeperutils "github.com/sei-protocol/sei-chain/x/dex/keeper/utils"
	"github.com/sei-protocol/sei-chain/x/dex/migrations"
	"github.com/sei-protocol/sei-chain/x/dex/types"
	dextypeswasm "github.com/sei-protocol/sei-chain/x/dex/types/wasm"
	"github.com/sei-protocol/sei-chain/x/store"
)

var (
	_ module.AppModule      = AppModule{}
	_ module.AppModuleBasic = AppModuleBasic{}
)

// ----------------------------------------------------------------------------
// AppModuleBasic
// ----------------------------------------------------------------------------

// AppModuleBasic implements the AppModuleBasic interface for the capability module.
type AppModuleBasic struct {
	cdc codec.BinaryCodec
}

func NewAppModuleBasic(cdc codec.BinaryCodec) AppModuleBasic {
	return AppModuleBasic{cdc: cdc}
}

// Name returns the capability module's name.
func (AppModuleBasic) Name() string {
	return types.ModuleName
}

func (AppModuleBasic) RegisterCodec(cdc *codec.LegacyAmino) {
	types.RegisterCodec(cdc)
}

func (AppModuleBasic) RegisterLegacyAminoCodec(cdc *codec.LegacyAmino) {
	types.RegisterCodec(cdc)
}

// RegisterInterfaces registers the module's interface types
func (a AppModuleBasic) RegisterInterfaces(reg cdctypes.InterfaceRegistry) {
	types.RegisterInterfaces(reg)
}

// DefaultGenesis returns the capability module's default genesis state.
func (AppModuleBasic) DefaultGenesis(cdc codec.JSONCodec) json.RawMessage {
	return cdc.MustMarshalJSON(types.DefaultGenesis())
}

// ValidateGenesis performs genesis state validation for the capability module.
func (AppModuleBasic) ValidateGenesis(cdc codec.JSONCodec, config client.TxEncodingConfig, bz json.RawMessage) error {
	var genState types.GenesisState
	if err := cdc.UnmarshalJSON(bz, &genState); err != nil {
		return fmt.Errorf("failed to unmarshal %s genesis state: %w", types.ModuleName, err)
	}
	return genState.Validate()
}

// RegisterRESTRoutes registers the capability module's REST service handlers.
func (AppModuleBasic) RegisterRESTRoutes(clientCtx client.Context, rtr *mux.Router) {
}

// RegisterGRPCGatewayRoutes registers the gRPC Gateway routes for the module.
func (AppModuleBasic) RegisterGRPCGatewayRoutes(clientCtx client.Context, mux *runtime.ServeMux) {
	types.RegisterQueryHandlerClient(context.Background(), mux, types.NewQueryClient(clientCtx)) //nolint:errcheck // this is inside a module, and the method doesn't return error.  Leave it alone.
}

// GetTxCmd returns the capability module's root tx command.
func (a AppModuleBasic) GetTxCmd() *cobra.Command {
	return tx.GetTxCmd()
}

// GetQueryCmd returns the capability module's root query command.
func (AppModuleBasic) GetQueryCmd() *cobra.Command {
	return query.GetQueryCmd(types.StoreKey)
}

// ----------------------------------------------------------------------------
// AppModule
// ----------------------------------------------------------------------------

// AppModule implements the AppModule interface for the capability module.
type AppModule struct {
	AppModuleBasic

	keeper        keeper.Keeper
	accountKeeper types.AccountKeeper
	bankKeeper    types.BankKeeper
	wasmKeeper    wasm.Keeper

	abciWrapper dexkeeperabci.KeeperWrapper

	tracingInfo *tracing.Info
}

func NewAppModule(
	cdc codec.Codec,
	keeper keeper.Keeper,
	accountKeeper types.AccountKeeper,
	bankKeeper types.BankKeeper,
	wasmKeeper wasm.Keeper,
	tracingInfo *tracing.Info,
) AppModule {
	return AppModule{
		AppModuleBasic: NewAppModuleBasic(cdc),
		keeper:         keeper,
		accountKeeper:  accountKeeper,
		bankKeeper:     bankKeeper,
		wasmKeeper:     wasmKeeper,
		abciWrapper:    dexkeeperabci.KeeperWrapper{Keeper: &keeper},
		tracingInfo:    tracingInfo,
	}
}

// Name returns the capability module's name.
func (am AppModule) Name() string {
	return am.AppModuleBasic.Name()
}

// Route returns the capability module's message routing key.
func (am AppModule) Route() sdk.Route {
	return sdk.NewRoute(types.RouterKey, NewHandler(am.keeper))
}

// QuerierRoute returns the capability module's query routing key.
func (AppModule) QuerierRoute() string { return types.QuerierRoute }

// LegacyQuerierHandler returns the capability module's Querier.
func (am AppModule) LegacyQuerierHandler(legacyQuerierCdc *codec.LegacyAmino) sdk.Querier {
	return nil
}

// RegisterServices registers a GRPC query service to respond to the
// module-specific GRPC queries.
func (am AppModule) RegisterServices(cfg module.Configurator) {
	types.RegisterMsgServer(cfg.MsgServer(), msgserver.NewMsgServerImpl(am.keeper))
	types.RegisterQueryServer(cfg.QueryServer(), dexkeeperquery.KeeperWrapper{Keeper: &am.keeper})

	_ = cfg.RegisterMigration(types.ModuleName, 1, func(ctx sdk.Context) error { return nil })
	_ = cfg.RegisterMigration(types.ModuleName, 2, func(ctx sdk.Context) error {
		return migrations.DataTypeUpdate(ctx, am.keeper.GetStoreKey(), am.keeper.Cdc)
	})
	_ = cfg.RegisterMigration(types.ModuleName, 3, func(ctx sdk.Context) error {
		return migrations.PriceSnapshotUpdate(ctx, am.keeper.Paramstore)
	})
	_ = cfg.RegisterMigration(types.ModuleName, 4, func(ctx sdk.Context) error {
		return migrations.V4ToV5(ctx, am.keeper.GetStoreKey(), am.keeper.Paramstore)
	})
	_ = cfg.RegisterMigration(types.ModuleName, 5, func(ctx sdk.Context) error {
		return migrations.V5ToV6(ctx, am.keeper.GetStoreKey(), am.keeper.Cdc)
	})
}

// RegisterInvariants registers the capability module's invariants.
func (am AppModule) RegisterInvariants(_ sdk.InvariantRegistry) {}

// InitGenesis performs the capability module's genesis initialization It returns
// no validator updates.
func (am AppModule) InitGenesis(ctx sdk.Context, cdc codec.JSONCodec, gs json.RawMessage) []abci.ValidatorUpdate {
	var genState types.GenesisState
	// Initialize global index to index in genesis state
	cdc.MustUnmarshalJSON(gs, &genState)

	InitGenesis(ctx, am.keeper, genState)

	return []abci.ValidatorUpdate{}
}

// ExportGenesis returns the capability module's exported genesis state as raw JSON bytes.
func (am AppModule) ExportGenesis(ctx sdk.Context, cdc codec.JSONCodec) json.RawMessage {
	genState := ExportGenesis(ctx, am.keeper)
	return cdc.MustMarshalJSON(genState)
}

// ConsensusVersion implements ConsensusVersion.
func (AppModule) ConsensusVersion() uint64 { return 6 }

func (am AppModule) getAllContractInfo(ctx sdk.Context) []types.ContractInfo {
	return am.keeper.GetAllContractInfo(ctx)
}

// BeginBlock executes all ABCI BeginBlock logic respective to the capability module.
func (am AppModule) BeginBlock(ctx sdk.Context, _ abci.RequestBeginBlock) {
	// TODO (codchen): Revert before mainnet so we don't silently fail on errors
	defer func() {
		if err := recover(); err != nil {
			ctx.Logger().Error(fmt.Sprintf("panic occurred in %s BeginBlock: %s", types.ModuleName, err))
			telemetry.IncrCounterWithLabels(
				[]string{fmt.Sprintf("%s%s", types.ModuleName, "beginblockpanic")},
				1,
				[]metrics.Label{
					telemetry.NewLabel("error", fmt.Sprintf("%s", err)),
				},
			)
		}
	}()

	am.keeper.MemState.Clear()
	isNewEpoch, currentEpoch := am.keeper.IsNewEpoch(ctx)
	if isNewEpoch {
		am.keeper.SetEpoch(ctx, currentEpoch)
	}
	for _, contract := range am.getAllContractInfo(ctx) {
		am.beginBlockForContract(ctx, contract, int64(currentEpoch))
	}
}

func (am AppModule) beginBlockForContract(ctx sdk.Context, contract types.ContractInfo, epoch int64) {
	_, span := (*am.tracingInfo.Tracer).Start(am.tracingInfo.TracerContext, "DexBeginBlock")
	contractAddr := contract.ContractAddr
	span.SetAttributes(attribute.String("contract", contractAddr))
	defer span.End()

	if contract.NeedHook {
		if err := am.abciWrapper.HandleBBNewBlock(ctx, contractAddr, epoch); err != nil {
			ctx.Logger().Error(fmt.Sprintf("New block hook error for %s: %s", contractAddr, err.Error()))
		}
	}

	if contract.NeedOrderMatching {
		currentTimestamp := uint64(ctx.BlockTime().Unix())
		ctx.Logger().Info(fmt.Sprintf("Removing stale prices for ts %d", currentTimestamp))
		priceRetention := am.keeper.GetParams(ctx).PriceSnapshotRetention
		for _, pair := range am.keeper.GetAllRegisteredPairs(ctx, contractAddr) {
			am.keeper.DeletePriceStateBefore(ctx, contractAddr, currentTimestamp-priceRetention, pair)
		}
	}
}

// EndBlock executes all ABCI EndBlock logic respective to the capability module. It
// returns no validator updates.
func (am AppModule) EndBlock(ctx sdk.Context, _ abci.RequestEndBlock) (ret []abci.ValidatorUpdate) {
	// TODO (codchen): Revert https://github.com/sei-protocol/sei-chain/pull/176/files before mainnet so we don't silently fail on errors
	defer func() {
		if err := recover(); err != nil {
			ctx.Logger().Error(fmt.Sprintf("panic occurred in %s EndBlock: %s", types.ModuleName, err))
			telemetry.IncrCounterWithLabels(
				[]string{fmt.Sprintf("%s%s", types.ModuleName, "endblockpanic")},
				1,
				[]metrics.Label{
					telemetry.NewLabel("error", fmt.Sprintf("%s", err)),
				},
			)
			ret = []abci.ValidatorUpdate{}
		}
	}()

	validContractAddresses := map[string]types.ContractInfo{}
	for _, contractInfo := range am.getAllContractInfo(ctx) {
		validContractAddresses[contractInfo.ContractAddr] = contractInfo
	}
	// Each iteration is atomic. If an iteration finishes without any error, it will return,
	// otherwise it will rollback any state change, filter out contracts that cause the error,
	// and proceed to the next iteration. The loop is guaranteed to finish since
	// `validContractAddresses` will always decrease in size every iteration.
	iterCounter := len(validContractAddresses)
	for len(validContractAddresses) > 0 {
		failedContractAddresses := datastructures.NewSyncSet([]string{})
		cachedCtx, msCached := store.GetCachedContext(ctx)
		// cache keeper in-memory state
		memStateCopy := am.keeper.MemState.DeepCopy()
		finalizeBlockMessages := sync.Map{} // of type map[string]*dextypeswasm.SudoFinalizeBlockMsg{}
		settlementsByContract := sync.Map{} // of type map[string][]*types.SettlementEntry{}
		for contractAddr := range validContractAddresses {
			finalizeBlockMessages.Store(contractAddr, dextypeswasm.NewSudoFinalizeBlockMsg())
			settlementsByContract.Store(contractAddr, []*types.SettlementEntry{})
		}

		validContractsInfo := []types.ContractInfo{}
		for _, contractInfo := range validContractAddresses {
			validContractsInfo = append(validContractsInfo, contractInfo)
		}
		// Handle deposit sequentially since they mutate `bank` state which is shared by all contracts
		keeperWrapper := dexkeeperabci.KeeperWrapper{Keeper: &am.keeper}
		for contractAddr := range validContractAddresses {
			if err := keeperWrapper.HandleEBDeposit(cachedCtx.Context(), cachedCtx, am.tracingInfo.Tracer, contractAddr); err != nil {
				failedContractAddresses.Add(contractAddr)
			}
		}

		mu := sync.Mutex{}
		runnable := func(contractInfo types.ContractInfo) {
			defer func() {
				if err := recover(); err != nil {
					ctx.Logger().Error(fmt.Sprintf("panic occurred during order matching for contract: %s", contractInfo.ContractAddr))
					telemetry.IncrCounterWithLabels(
						[]string{fmt.Sprintf("%s%s", types.ModuleName, "endblockpanic")},
						1,
						[]metrics.Label{
							telemetry.NewLabel("error", fmt.Sprintf("%s", err)),
							telemetry.NewLabel("contract", contractInfo.ContractAddr),
						},
					)
					// idempotent
					failedContractAddresses.Add(contractInfo.ContractAddr)
				}
			}()

			if !contractInfo.NeedOrderMatching {
				return
			}
			cachedCtx.Logger().Info(fmt.Sprintf("End block for %s", contractInfo.ContractAddr))
			if orderResultsMap, settlements, err := contract.HandleExecutionForContract(cachedCtx, contractInfo, &am.keeper, am.tracingInfo.Tracer); err != nil {
				cachedCtx.Logger().Error(fmt.Sprintf("Error for EndBlock of %s", contractInfo.ContractAddr))
				failedContractAddresses.Add(contractInfo.ContractAddr)
			} else {
				for account, orderResults := range orderResultsMap {
					// only add to finalize message for contract addresses
					if msg, ok := finalizeBlockMessages.Load(account); ok {
						typedMsg := msg.(*dextypeswasm.SudoFinalizeBlockMsg)
						mu.Lock()
						typedMsg.AddContractResult(orderResults)
						mu.Unlock()
					}
				}
				settlementsByContract.Store(contractInfo.ContractAddr, settlements)
			}
		}

		runner := contract.NewParallelRunner(runnable, validContractsInfo)
		runner.Run()

		settlementsByContract.Range(func(key, val any) bool {
			contractAddr := key.(string)
			settlements := val.([]*types.SettlementEntry)
			if !validContractAddresses[contractAddr].NeedOrderMatching {
				return true
			}
			if err := contract.HandleSettlements(cachedCtx, contractAddr, &am.keeper, settlements); err != nil {
				ctx.Logger().Error(fmt.Sprintf("Error handling settlements for %s", contractAddr))
				failedContractAddresses.Add(contractAddr)
			}
			return true
		})

		finalizeBlockMessages.Range(func(key, val any) bool {
			contractAddr := key.(string)
			finalizeBlockMsg := val.(*dextypeswasm.SudoFinalizeBlockMsg)
			if !validContractAddresses[contractAddr].NeedHook {
				return true
			}
			if _, err := dexkeeperutils.CallContractSudo(cachedCtx, &am.keeper, contractAddr, finalizeBlockMsg); err != nil {
				cachedCtx.Logger().Error(fmt.Sprintf("Error calling FinalizeBlock of %s", contractAddr))
				failedContractAddresses.Add(contractAddr)
			}
			return true
		})

		// No error is thrown for any contract. This should happen most of the time.
		if failedContractAddresses.Size() == 0 {
			msCached.Write()
			return []abci.ValidatorUpdate{}
		}
		// restore keeper in-memory state
		*am.keeper.MemState = *memStateCopy
		// exclude orders by failed contracts from in-memory state,
		// then update `validContractAddresses`
		for _, failedContractAddress := range failedContractAddresses.ToOrderedSlice(datastructures.StringComparator) {
			am.keeper.MemState.DeepFilterAccount(failedContractAddress)
			delete(validContractAddresses, failedContractAddress)
		}

		iterCounter--
		if iterCounter == 0 {
			ctx.Logger().Error("All contracts failed in dex EndBlock. Doing nothing.")
			break
		}
	}

	// don't call `ctx.Write` if all contracts have error
	return []abci.ValidatorUpdate{}
}
