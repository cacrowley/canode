package app

import (
	"encoding/json"
	"fmt"
	"github.com/tendermint/iavl"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"time"

	"github.com/cosmos/cosmos-sdk/baseapp"
	storePkg "github.com/cosmos/cosmos-sdk/store"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/stake"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/tmhash"
	cmn "github.com/tendermint/tendermint/libs/common"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/binance-chain/node/admin"
	"github.com/binance-chain/node/app/config"
	"github.com/binance-chain/node/app/pub"
	"github.com/binance-chain/node/app/val"
	"github.com/binance-chain/node/common"
	"github.com/binance-chain/node/common/fees"
	bnclog "github.com/binance-chain/node/common/log"
	"github.com/binance-chain/node/common/runtime"
	"github.com/binance-chain/node/common/tx"
	"github.com/binance-chain/node/common/types"
	"github.com/binance-chain/node/common/utils"
	"github.com/binance-chain/node/plugins/dex"
	"github.com/binance-chain/node/plugins/dex/order"
	"github.com/binance-chain/node/plugins/ico"
	"github.com/binance-chain/node/plugins/param"
	"github.com/binance-chain/node/plugins/param/paramhub"
	"github.com/binance-chain/node/plugins/tokens"
	tkstore "github.com/binance-chain/node/plugins/tokens/store"
	"github.com/binance-chain/node/wire"
)

const (
	appName = "BNBChain"
)

// default home directories for expected binaries
var (
	DefaultCLIHome      = os.ExpandEnv("$HOME/.bnbcli")
	DefaultNodeHome     = os.ExpandEnv("$HOME/.bnbchaind")
	Bech32PrefixAccAddr string
)

// BinanceChain implements ChainApp
var _ types.ChainApp = (*BinanceChain)(nil)

var (
	Codec         = MakeCodec()
	ServerContext = config.NewDefaultContext()
)

// BinanceChain is the BNBChain ABCI application
type BinanceChain struct {
	*baseapp.BaseApp
	Codec *wire.Codec

	// the abci query handler mapping is `prefix -> handler`
	queryHandlers map[string]types.AbciQueryHandler

	// keepers
	CoinKeeper    bank.Keeper
	DexKeeper     *dex.DexKeeper
	AccountKeeper auth.AccountKeeper
	TokenMapper   tkstore.Mapper
	ValAddrMapper val.Mapper
	stakeKeeper   stake.Keeper
	govKeeper     gov.Keeper
	// keeper to process param store and update
	ParamHub *param.ParamHub

	baseConfig        *config.BaseConfig
	publicationConfig *config.PublicationConfig
	publisher         pub.MarketDataPublisher

	// Unlike tendermint, we don't need implement a no-op metrics, usage of this field should
	// check nil-ness to know whether metrics collection is turn on
	// TODO(#246): make it an aggregated wrapper of all component metrics (i.e. DexKeeper, StakeKeeper)
	metrics *pub.Metrics

	stateSyncHeight     int64
	stateSyncNumKeys    []int64
	stateSyncStoreInfos []storePkg.StoreInfo
}

// NewBinanceChain creates a new instance of the BinanceChain.
func NewBinanceChain(logger log.Logger, db dbm.DB, traceStore io.Writer, baseAppOptions ...func(*baseapp.BaseApp)) *BinanceChain {
	// set running mode from cmd parameter or config
	err := runtime.SetRunningMode(runtime.Mode(ServerContext.StartMode))
	if err != nil {
		cmn.Exit(err.Error())
	}
	// create app-level codec for txs and accounts
	var cdc = Codec
	// create composed tx decoder
	decoders := wire.ComposeTxDecoders(cdc, defaultTxDecoder)

	// create the applicationsimulate object
	var app = &BinanceChain{
		BaseApp:           baseapp.NewBaseApp(appName /*, cdc*/, logger, db, decoders, sdk.CollectConfig{ServerContext.PublishAccountBalance, ServerContext.PublishTransfer}, baseAppOptions...),
		Codec:             cdc,
		queryHandlers:     make(map[string]types.AbciQueryHandler),
		baseConfig:        ServerContext.BaseConfig,
		publicationConfig: ServerContext.PublicationConfig,
	}

	app.SetCommitMultiStoreTracer(traceStore)

	// mappers
	app.AccountKeeper = auth.NewAccountKeeper(cdc, common.AccountStoreKey, types.ProtoAppAccount)
	app.TokenMapper = tkstore.NewMapper(cdc, common.TokenStoreKey)
	app.ValAddrMapper = val.NewMapper(common.ValAddrStoreKey)
	app.CoinKeeper = bank.NewBaseKeeper(app.AccountKeeper)
	app.ParamHub = paramhub.NewKeeper(cdc, common.ParamsStoreKey, common.TParamsStoreKey)
	app.stakeKeeper = stake.NewKeeper(
		cdc,
		common.StakeStoreKey, common.TStakeStoreKey,
		app.CoinKeeper, app.ParamHub.Subspace(stake.DefaultParamspace),
		app.RegisterCodespace(stake.DefaultCodespace),
	)
	app.govKeeper = gov.NewKeeper(
		cdc,
		common.GovStoreKey,
		app.ParamHub.Keeper, app.ParamHub.Subspace(gov.DefaultParamspace), app.CoinKeeper, app.stakeKeeper,
		app.RegisterCodespace(gov.DefaultCodespace),
		app.Pool,
	)
	app.ParamHub.SetGovKeeper(app.govKeeper)
	// legacy bank route (others moved to plugin init funcs)
	app.Router().
		AddRoute("bank", bank.NewHandler(app.CoinKeeper)).
		AddRoute("stake", stake.NewHandler(app.stakeKeeper, app.govKeeper)).
		AddRoute("gov", gov.NewHandler(app.govKeeper))

	app.QueryRouter().AddRoute("gov", gov.NewQuerier(app.govKeeper))
	app.RegisterQueryHandler("account", app.AccountHandler)
	app.RegisterQueryHandler("admin", admin.GetHandler(ServerContext.Config))

	if ServerContext.Config.Instrumentation.Prometheus {
		app.metrics = pub.PrometheusMetrics() // TODO(#246): make it an aggregated wrapper of all component metrics (i.e. DexKeeper, StakeKeeper)
	}

	if app.publicationConfig.ShouldPublishAny() {
		pub.Logger = logger.With("module", "pub")
		pub.Cfg = app.publicationConfig
		pub.ToPublishCh = make(chan pub.BlockInfoToPublish, app.publicationConfig.PublicationChannelSize)

		publishers := make([]pub.MarketDataPublisher, 0, 1)
		if app.publicationConfig.PublishKafka {
			publishers = append(publishers, pub.NewKafkaMarketDataPublisher(app.Logger))
		}
		if app.publicationConfig.PublishLocal {
			publishers = append(publishers, pub.NewLocalMarketDataPublisher(ServerContext.Config.RootDir, app.Logger, app.publicationConfig))
		}

		if len(publishers) == 0 {
			panic(fmt.Errorf("Cannot find any publisher in config, there might be some wrong configuration"))
		} else {
			if len(publishers) == 1 {
				app.publisher = publishers[0]
			} else {
				app.publisher = pub.NewAggregatedMarketDataPublisher(publishers...)
			}

			go pub.Publish(app.publisher, app.metrics, logger, app.publicationConfig, pub.ToPublishCh)
			pub.IsLive = true
		}
	}

	// finish app initialization
	app.SetInitChainer(app.initChainerFn())
	app.SetEndBlocker(app.EndBlocker)
	app.MountStoresIAVL(
		common.MainStoreKey,
		common.AccountStoreKey,
		common.ValAddrStoreKey,
		common.TokenStoreKey,
		common.DexStoreKey,
		common.PairStoreKey,
		common.ParamsStoreKey,
		common.StakeStoreKey,
		common.GovStoreKey,
	)
	app.SetAnteHandler(tx.NewAnteHandler(app.AccountKeeper))
	app.SetPreChecker(tx.NewTxPreChecker())
	app.MountStoresTransient(common.TParamsStoreKey, common.TStakeStoreKey)

	// block store required to hydrate dex OB
	err = app.LoadCMSLatestVersion()
	if err != nil {
		cmn.Exit(err.Error())
	}

	// init app cache
	accountStore := app.BaseApp.GetCommitMultiStore().GetKVStore(common.AccountStoreKey)
	app.SetAccountStoreCache(cdc, accountStore, app.baseConfig.AccountCacheSize)

	tx.InitSigCache(app.baseConfig.SignatureCacheSize)

	err = app.InitFromStore(common.MainStoreKey)
	if err != nil {
		cmn.Exit(err.Error())
	}

	// remaining plugin init
	app.initDex()
	app.initPlugins()
	app.initParams()
	return app
}

func (app *BinanceChain) initDex() {
	tradingPairMapper := dex.NewTradingPairMapper(app.Codec, common.PairStoreKey)
	app.DexKeeper = dex.NewOrderKeeper(common.DexStoreKey, app.AccountKeeper, tradingPairMapper,
		app.RegisterCodespace(dex.DefaultCodespace), app.baseConfig.OrderKeeperConcurrency, app.Codec,
		app.publicationConfig.ShouldPublishAny())
	app.DexKeeper.SubscribeParamChange(app.ParamHub)
	// do not proceed if we are in a unit test and `CheckState` is unset.
	if app.CheckState == nil {
		return
	}
	// count back to days in config.
	app.DexKeeper.InitOrderBook(app.CheckState.Ctx, app.baseConfig.BreatheBlockDaysCountBack,
		baseapp.LoadBlockDB(), baseapp.LoadTxDB(), app.LastBlockHeight(), app.TxDecoder)
}

func (app *BinanceChain) initPlugins() {
	tokens.InitPlugin(app, app.TokenMapper, app.AccountKeeper, app.CoinKeeper)
	dex.InitPlugin(app, app.DexKeeper, app.TokenMapper, app.AccountKeeper, app.govKeeper)
	param.InitPlugin(app, app.ParamHub)
}

func (app *BinanceChain) initParams() {
	if app.CheckState == nil || app.CheckState.Ctx.BlockHeight() == 0 {
		return
	}
	app.ParamHub.Load(app.CheckState.Ctx)
}

// initChainerFn performs custom logic for chain initialization.
func (app *BinanceChain) initChainerFn() sdk.InitChainer {
	return func(ctx sdk.Context, req abci.RequestInitChain) abci.ResponseInitChain {
		stateJSON := req.AppStateBytes

		genesisState := new(GenesisState)
		err := app.Codec.UnmarshalJSON(stateJSON, genesisState)
		if err != nil {
			panic(err) // TODO https://github.com/cosmos/cosmos-sdk/issues/468
			// return sdk.ErrGenesisParse("").TraceCause(err, "")
		}

		validatorAddrs := make([]sdk.AccAddress, len(genesisState.Accounts))
		for i, gacc := range genesisState.Accounts {
			acc := gacc.ToAppAccount()
			acc.AccountNumber = app.AccountKeeper.GetNextAccountNumber(ctx)
			app.AccountKeeper.SetAccount(ctx, acc)
			app.ValAddrMapper.SetVal(ctx, gacc.ValAddr, gacc.Address)
			validatorAddrs[i] = acc.Address
		}
		tokens.InitGenesis(ctx, app.TokenMapper, app.CoinKeeper, genesisState.Tokens,
			validatorAddrs, DefaultSelfDelegationToken.Amount)

		app.ParamHub.InitGenesis(ctx, genesisState.ParamGenesis)
		validators, err := stake.InitGenesis(ctx, app.stakeKeeper, genesisState.StakeData)
		gov.InitGenesis(ctx, app.govKeeper, genesisState.GovData)

		if err != nil {
			panic(err) // TODO find a way to do this w/o panics
		}

		// before we deliver the genTxs, we have transferred some delegation tokens to the validator candidates.
		if len(genesisState.GenTxs) > 0 {
			for _, genTx := range genesisState.GenTxs {
				var tx auth.StdTx
				err = app.Codec.UnmarshalJSON(genTx, &tx)
				if err != nil {
					panic(err)
				}
				bz := app.Codec.MustMarshalBinaryLengthPrefixed(tx)
				res := app.BaseApp.DeliverTx(bz)
				if !res.IsOK() {
					panic(res.Log)
				}
			}
			validators = app.stakeKeeper.ApplyAndReturnValidatorSetUpdates(ctx)
		}

		// sanity check
		if len(req.Validators) > 0 {
			if len(req.Validators) != len(validators) {
				panic(fmt.Errorf("genesis validator numbers are not matched, staked=%d, req=%d",
					len(validators), len(req.Validators)))
			}
			sort.Sort(abci.ValidatorUpdates(req.Validators))
			sort.Sort(abci.ValidatorUpdates(validators))
			for i, val := range validators {
				if !val.Equal(req.Validators[i]) {
					panic(fmt.Errorf("invalid genesis validator, index=%d", i))
				}
			}
		}

		return abci.ResponseInitChain{
			Validators: validators,
		}
	}
}

func (app *BinanceChain) CheckTx(txBytes []byte) (res abci.ResponseCheckTx) {
	var result sdk.Result
	var tx sdk.Tx
	// try to get the Tx first from cache, if succeed, it means it is PreChecked.
	tx, ok := app.GetTxFromCache(txBytes)
	if ok {
		if admin.IsTxAllowed(tx) {
			txHash := cmn.HexBytes(tmhash.Sum(txBytes)).String()
			app.Logger.Debug("Handle CheckTx", "Tx", txHash)
			result = app.RunTx(sdk.RunTxModeCheckAfterPre, txBytes, tx, txHash)
			if !result.IsOK() {
				app.RemoveTxFromCache(txBytes)
			}
		} else {
			result = admin.TxNotAllowedError().Result()
		}
	} else {
		tx, err := app.TxDecoder(txBytes)
		if err != nil {
			result = err.Result()
		} else {
			if admin.IsTxAllowed(tx) {
				txHash := cmn.HexBytes(tmhash.Sum(txBytes)).String()
				app.Logger.Debug("Handle CheckTx", "Tx", txHash)
				result = app.RunTx(sdk.RunTxModeCheck, txBytes, tx, txHash)
				if result.IsOK() {
					app.AddTxToCache(txBytes, tx)
				}
			} else {
				result = admin.TxNotAllowedError().Result()
			}
		}
	}

	return abci.ResponseCheckTx{
		Code: uint32(result.Code),
		Data: result.Data,
		Log:  result.Log,
		Tags: result.Tags,
	}
}

// Implements ABCI
func (app *BinanceChain) DeliverTx(txBytes []byte) (res abci.ResponseDeliverTx) {
	res = app.BaseApp.DeliverTx(txBytes)
	if res.IsOK() {
		// commit or panic
		fees.Pool.CommitFee(cmn.HexBytes(tmhash.Sum(txBytes)).String())
	} else {
		if app.publicationConfig.PublishOrderUpdates {
			app.processErrAbciResponseForPub(txBytes)
		}
	}

	return res
}

// PreDeliverTx implements extended ABCI for concurrency
// PreCheckTx would perform decoding, signture and other basic verification
func (app *BinanceChain) PreDeliverTx(txBytes []byte) (res abci.ResponseDeliverTx) {
	res = app.BaseApp.PreDeliverTx(txBytes)
	if res.IsErr() {
		txHash := cmn.HexBytes(tmhash.Sum(txBytes)).String()
		app.Logger.Error("failed to process invalid tx during pre-deliver", "tx", txHash, "res", res.String())
		// TODO(#446): comment out temporally for thread safety
		//if app.publicationConfig.PublishOrderUpdates {
		//	app.processErrAbciResponseForPub(txBytes)
		//}
	}
	return res
}

func (app *BinanceChain) isBreatheBlock(height int64, lastBlockTime time.Time, blockTime time.Time) bool {
	if app.baseConfig.BreatheBlockInterval > 0 {
		return height%int64(app.baseConfig.BreatheBlockInterval) == 0
	} else {
		return !utils.SameDayInUTC(lastBlockTime, blockTime)
	}
}

func (app *BinanceChain) EndBlocker(ctx sdk.Context, req abci.RequestEndBlock) abci.ResponseEndBlock {
	// lastBlockTime would be 0 if this is the first block.
	lastBlockTime := app.CheckState.Ctx.BlockHeader().Time
	blockTime := ctx.BlockHeader().Time
	height := ctx.BlockHeader().Height

	var tradesToPublish []*pub.Trade

	isBreatheBlock := app.isBreatheBlock(height, lastBlockTime, blockTime)
	if !isBreatheBlock || height == 1 {
		// only match in the normal block
		app.Logger.Debug("normal block", "height", height)
		if app.publicationConfig.ShouldPublishAny() && pub.IsLive {
			tradesToPublish = pub.MatchAndAllocateAllForPublish(app.DexKeeper, ctx)
		} else {
			app.DexKeeper.MatchAndAllocateAll(ctx, nil)
		}
	} else {
		// breathe block
		bnclog.Info("Start Breathe Block Handling",
			"height", height, "lastBlockTime", lastBlockTime, "newBlockTime", blockTime)
		icoDone := ico.EndBlockAsync(ctx)
		dex.EndBreatheBlock(ctx, app.DexKeeper, height, blockTime)
		param.EndBreatheBlock(ctx, app.ParamHub)
		// other end blockers
		<-icoDone
	}

	blockFee := distributeFee(ctx, app.AccountKeeper, app.ValAddrMapper, app.publicationConfig.PublishBlockFee)

	tags, passed, failed := gov.EndBlocker(ctx, app.govKeeper)
	var proposals pub.Proposals

	if app.publicationConfig.PublishOrderUpdates {
		proposals = pub.CollectProposalsForPublish(passed, failed)
	}

	if app.publicationConfig.ShouldPublishAny() &&
		pub.IsLive {
		if height >= app.publicationConfig.FromHeightInclusive {
			app.publish(tradesToPublish, &proposals, blockFee, ctx, height, blockTime.Unix())
		}

		// clean up intermediate cached data
		app.DexKeeper.ClearOrderChanges()
		app.DexKeeper.ClearRoundFee()
	}

	var validatorUpdates abci.ValidatorUpdates
	if isBreatheBlock {
		// some endblockers without fees will execute after publish to make publication run as early as possible.
		validatorUpdates = stake.EndBlocker(ctx, app.stakeKeeper)
	}

	//match may end with transaction failure, which is better to save into
	//the EndBlock response. However, current cosmos doesn't support this.
	//future TODO: add failure info.
	return abci.ResponseEndBlock{
		ValidatorUpdates: validatorUpdates,
		Tags:             tags,
	}
}

func (app *BinanceChain) LatestSnapshot() (height int64, numKeys []int64, err error) {
	app.Logger.Info("query latest snapshot")
	numKeys = make([]int64, 0, len(common.StoreKeyNames))
	height = app.LastBlockHeight()

	for _, key := range common.StoreKeyNames {
		var storeKeys int64
		store := app.GetCommitMultiStore().GetKVStore(common.StoreKeyNameMap[key])
		//itr := store.Iterator(nil, nil)
		//for ; itr.Valid(); itr.Next() {
		//	storeKeys++
		//}
		//numKeys = append(numKeys, storeKeys)

		mutableTree := store.(*storePkg.IavlStore).Tree
		if tree, err := mutableTree.GetImmutable(height); err == nil {
			tree.IterateFirst(func(nodeBytes []byte) {
				storeKeys++
			})
		} else {
			app.Logger.Error("failed to load immutable tree", "err", err)
		}
		numKeys = append(numKeys, storeKeys)
	}

	return
}

func (app *BinanceChain) ReadSnapshotChunk(height int64, startIndex, endIndex int64) (chunk [][]byte, err error) {
	app.Logger.Info("read snapshot chunk", "height", height, "startIndex", startIndex, "endIndex", endIndex)
	fmt.Printf("read snapshot height:%d\n", height)
	chunk = make([][]byte, 0, endIndex-startIndex)

	// TODO: can be optimized - direct jump to expected store
	iterated := int64(0)
	for _, key := range common.StoreKeyNames {
		store := app.GetCommitMultiStore().GetKVStore(common.StoreKeyNameMap[key])

		// No needed temporaly (in case inject too many interfaces)
		//itr := store.Iterator(nil, nil)
		//fmt.Printf("read snapshot store:%s\n", key)
		//for ; itr.Valid(); itr.Next() {
		//	ik := itr.Key()
		//	iv := itr.Value()
		//	keyValueBytes = append(keyValueBytes, ik)
		//	keyValueBytes = append(keyValueBytes, iv)
		//	fmt.Printf("read snapshot key:%X, value:%X\n", ik, iv)
		//}

		mutableTree := store.(*storePkg.IavlStore).Tree
		if tree, err := mutableTree.GetImmutable(height); err == nil {
			tree.IterateFirst(func(nodeBytes []byte) {
				if iterated >= startIndex && iterated < endIndex {
					chunk = append(chunk, nodeBytes)
				}
				iterated += 1
			})
		} else {
			app.Logger.Error("failed to load immutable tree", "err", err)
		}
	}

	if int64(len(chunk)) != (endIndex - startIndex) {
		app.Logger.Error("failed to load enough chunk", "expected", endIndex-startIndex, "got", len(chunk))
	}

	return
}

func (app *BinanceChain) StartRecovery(height int64, numKeys []int64) error {
	app.Logger.Info("start recovery")
	//for storeName, numKey := range numKeys {
	//	if numKey > 0 {
	//		store := app.GetCommitMultiStore().GetKVStore(common.StoreKeyNameMap[storeName])
	//		// pretend we have history
	//		store.(*storePkg.IavlStore).Tree.SetVersion(height - 1)
	//	}
	//}
	app.stateSyncHeight = height - 1
	app.stateSyncNumKeys = numKeys
	app.stateSyncStoreInfos = make([]storePkg.StoreInfo, 0)

	return nil
}

func (app *BinanceChain) WriteRecoveryChunk(chunk [][]byte) error {
	//store := app.GetCommitMultiStore().GetKVStore(common.StoreKeyNameMap[storeName])
	app.Logger.Info("start write recovery chunk")
	nodes := make([]*iavl.Node, 0)
	for idx := 0; idx < len(chunk); idx++ {
		node, _ := iavl.MakeNode(chunk[idx])
		node.Hash()
		nodes = append(nodes, node)
	}

	//if len(chunk) > 0 {
	//	mutableTree := store.(*store.IavlStore).Tree
	//	tree := mutableTree.ImmutableTree
	//	tree.RecoverFromRemoteNodes(nodes, inOrderKeys)
	//	// load a full tree from db to generate dot graph
	//	//tree.Iterate(func(key []byte, value []byte) bool {
	//	//	return false
	//	//})
	//	tree.Hash()
	//
	//	file, _ := os.Create(fmt.Sprintf("/Users/zhaocong/trees_witness/%s.txt", storeName))
	//	iavl.WriteDOTGraph(file, tree, []iavl.PathToLeaf{})
	//}

	iterated := int64(0)
	for storeIdx, storeName := range common.StoreKeyNames {
		db := dbm.NewPrefixDB(app.GetDB(), []byte("s/k:"+storeName+"/"))
		nodeDB := iavl.NewNodeDB(db, 10000)

		var nodeHash []byte
		storeKeys := app.stateSyncNumKeys[storeIdx]
		if storeKeys > 0 {
			nodeDB.SaveRoot(nodes[iterated], app.stateSyncHeight)
			nodeHash = nodes[iterated].Hash()
			for i := int64(0); i < storeKeys; i++ {
				node := nodes[iterated+i]
				fmt.Printf("saved node hash: %X\n", node.Hash())
				nodeDB.SaveNode(node)
			}
		} else {
			nodeDB.SaveEmptyRoot(app.stateSyncHeight)
			nodeHash = nil
		}

		app.stateSyncStoreInfos = append(app.stateSyncStoreInfos, storePkg.StoreInfo{
			Name: storeName,
			Core: storePkg.StoreCore{
				CommitID: storePkg.CommitID{
					Version: app.stateSyncHeight,
					Hash:    nodeHash,
				},
			},
		})

		fmt.Printf("commit store: %d, root hash: %X\n", storeName, nodeHash)
		nodeDB.Commit()
		iterated += storeKeys
	}

	app.Logger.Info("finished write recovery chunk")
	return nil
}

func (app *BinanceChain) EndRecovery(height int64) error {
	app.Logger.Info("finished recovery")
	//stores := app.GetCommitMultiStore()
	//commitId := stores.CommitAt(height)
	// block store required to hydrate dex OB

	// simulate setLatestversion key
	batch := app.GetDB().NewBatch()
	latestBytes, _ := app.Codec.MarshalBinaryLengthPrefixed(height) // Does not error
	batch.Set([]byte("s/latest"), latestBytes)

	// simulate setCommitInfo
	fmt.Printf("!!!cong!!!version=%d\n", height)

	ci := storePkg.CommitInfo{
		Version:    height,
		StoreInfos: app.stateSyncStoreInfos,
	}
	cInfoBytes, err := app.Codec.MarshalBinaryLengthPrefixed(ci)
	if err != nil {
		panic(err)
	}
	cInfoKey := fmt.Sprintf("s/%d", height)
	batch.Set([]byte(cInfoKey), cInfoBytes)
	batch.WriteSync()

	// load into memory from db
	err = app.LoadCMSLatestVersion()
	if err != nil {
		cmn.Exit(err.Error())
	}
	stores := app.GetCommitMultiStore()
	commitId := stores.LastCommitID()
	hashHex := fmt.Sprintf("%X", commitId.Hash)
	app.Logger.Info("commit by state reactor", "version", commitId.Version, "hash", hashHex)
	return nil
}

// ExportAppStateAndValidators exports blockchain world state to json.
func (app *BinanceChain) ExportAppStateAndValidators() (appState json.RawMessage, validators []tmtypes.GenesisValidator, err error) {
	ctx := app.NewContext(sdk.RunTxModeCheck, abci.Header{})

	// iterate to get the accounts
	accounts := []GenesisAccount{}
	appendAccount := func(acc sdk.Account) (stop bool) {
		account := GenesisAccount{
			Address: acc.GetAddress(),
		}
		accounts = append(accounts, account)
		return false
	}
	app.AccountKeeper.IterateAccounts(ctx, appendAccount)

	genState := GenesisState{
		Accounts: accounts,
	}
	appState, err = wire.MarshalJSONIndent(app.Codec, genState)
	if err != nil {
		return nil, nil, err
	}
	return appState, validators, nil
}

// Query performs an abci query.
func (app *BinanceChain) Query(req abci.RequestQuery) (res abci.ResponseQuery) {
	defer func() {
		if r := recover(); r != nil {
			app.Logger.Error("internal error caused by query", "req", req, "stack", debug.Stack())
			res = sdk.ErrInternal("internal error").QueryResult()
		}
	}()

	path := baseapp.SplitPath(req.Path)
	if len(path) == 0 {
		msg := "no query path provided"
		return sdk.ErrUnknownRequest(msg).QueryResult()
	}
	prefix := path[0]
	if handler, ok := app.queryHandlers[prefix]; ok {
		res := handler(app, req, path)
		if res == nil {
			return app.BaseApp.Query(req)
		}
		return *res
	}
	return app.BaseApp.Query(req)
}

func (app *BinanceChain) AccountHandler(chainApp types.ChainApp, req abci.RequestQuery, path []string) *abci.ResponseQuery {
	var res abci.ResponseQuery
	if len(path) == 2 {
		addr := path[1]
		if accAddress, err := sdk.AccAddressFromBech32(addr); err == nil {

			if acc := app.CheckState.AccountCache.GetAccount(accAddress); acc != nil {
				bz, err := Codec.MarshalBinaryBare(acc)
				if err != nil {
					res = sdk.ErrInvalidAddress(addr).QueryResult()
				} else {
					res = abci.ResponseQuery{
						Code:  uint32(sdk.ABCICodeOK),
						Value: bz,
					}
				}
			} else {
				// let api server return 204 No Content
				res = abci.ResponseQuery{
					Code:  uint32(sdk.ABCICodeOK),
					Value: make([]byte, 0, 0),
				}
			}
		} else {
			res = sdk.ErrInvalidAddress(addr).QueryResult()
		}
	} else {
		res = sdk.ErrUnknownRequest("invalid path").QueryResult()
	}
	return &res
}

// RegisterQueryHandler registers an abci query handler, implements ChainApp.RegisterQueryHandler.
func (app *BinanceChain) RegisterQueryHandler(prefix string, handler types.AbciQueryHandler) {
	if _, ok := app.queryHandlers[prefix]; ok {
		panic(fmt.Errorf("registerQueryHandler: prefix `%s` is already registered", prefix))
	} else {
		app.queryHandlers[prefix] = handler
	}
}

// GetCodec returns the app's Codec.
func (app *BinanceChain) GetCodec() *wire.Codec {
	return app.Codec
}

// GetRouter returns the app's Router.
func (app *BinanceChain) GetRouter() baseapp.Router {
	return app.Router()
}

// GetContextForCheckState gets the context for the check state.
func (app *BinanceChain) GetContextForCheckState() sdk.Context {
	return app.CheckState.Ctx
}

// default custom logic for transaction decoding
func defaultTxDecoder(cdc *wire.Codec) sdk.TxDecoder {
	return func(txBytes []byte) (sdk.Tx, sdk.Error) {
		var tx = auth.StdTx{}

		if len(txBytes) == 0 {
			return nil, sdk.ErrTxDecode("txBytes are empty")
		}

		// StdTx.Msg is an interface. The concrete types
		// are registered by MakeTxCodec
		err := cdc.UnmarshalBinaryLengthPrefixed(txBytes, &tx)
		if err != nil {
			return nil, sdk.ErrTxDecode("").TraceSDK(err.Error())
		}
		return tx, nil
	}
}

// MakeCodec creates a custom tx codec.
func MakeCodec() *wire.Codec {
	var cdc = wire.NewCodec()

	wire.RegisterCrypto(cdc) // Register crypto.
	bank.RegisterCodec(cdc)
	sdk.RegisterCodec(cdc) // Register Msgs
	dex.RegisterWire(cdc)
	tokens.RegisterWire(cdc)
	types.RegisterWire(cdc)
	tx.RegisterWire(cdc)
	stake.RegisterCodec(cdc)
	gov.RegisterCodec(cdc)
	param.RegisterWire(cdc)
	return cdc
}

func (app *BinanceChain) publish(tradesToPublish []*pub.Trade, proposalsToPublish *pub.Proposals, blockFee pub.BlockFee, ctx sdk.Context, height, blockTime int64) {
	pub.Logger.Info("start to collect publish information", "height", height)

	var accountsToPublish map[string]pub.Account
	var transferToPublish *pub.Transfers
	var latestPriceLevels order.ChangedPriceLevelsMap

	duration := pub.Timer(app.Logger, fmt.Sprintf("collect publish information, height=%d", height), func() {
		if app.publicationConfig.PublishAccountBalance {
			txRelatedAccounts := app.Pool.TxRelatedAddrs()
			tradeRelatedAccounts := pub.GetTradeAndOrdersRelatedAccounts(app.DexKeeper, tradesToPublish)
			accountsToPublish = pub.GetAccountBalances(
				app.AccountKeeper,
				ctx,
				txRelatedAccounts,
				tradeRelatedAccounts,
				blockFee.Validators)
		}
		if app.publicationConfig.PublishTransfer {
			transferToPublish = pub.GetTransferPublished(app.Pool, height, blockTime)
		}

		if app.publicationConfig.PublishOrderBook {
			latestPriceLevels = app.DexKeeper.GetOrderBooks(pub.MaxOrderBookLevel)
		}
	})

	if app.metrics != nil {
		app.metrics.CollectBlockTimeMs.Set(float64(duration))
	}

	pub.Logger.Info("start to publish", "height", height,
		"blockTime", blockTime, "numOfTrades", len(tradesToPublish),
		"numOfOrders", // the order num we collected here doesn't include trade related orders
		len(app.DexKeeper.OrderChanges),
		"numOfProposals",
		proposalsToPublish.NumOfMsgs,
		"numOfAccounts",
		len(accountsToPublish))
	pub.ToRemoveOrderIdCh = make(chan string, pub.ToRemoveOrderIdChannelSize)
	pub.ToPublishCh <- pub.NewBlockInfoToPublish(
		height,
		blockTime,
		tradesToPublish,
		proposalsToPublish,
		app.DexKeeper.OrderChanges,     // thread-safety is guarded by the signal from RemoveDoneCh
		app.DexKeeper.OrderInfosForPub, // thread-safety is guarded by the signal from RemoveDoneCh
		accountsToPublish,
		latestPriceLevels,
		blockFee,
		app.DexKeeper.RoundOrderFees,
		transferToPublish)

	// remove item from OrderInfoForPublish when we published removed order (cancel, iocnofill, fullyfilled, expired)
	for id := range pub.ToRemoveOrderIdCh {
		pub.Logger.Debug("delete order from order changes map", "orderId", id)
		delete(app.DexKeeper.OrderInfosForPub, id)
	}

	pub.Logger.Debug("finish publish", "height", height)
}
