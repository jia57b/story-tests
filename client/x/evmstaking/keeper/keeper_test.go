package keeper_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"cosmossdk.io/log"
	sdkmath "cosmossdk.io/math"
	"cosmossdk.io/store"
	"cosmossdk.io/store/metrics"
	storetypes "cosmossdk.io/store/types"

	"github.com/cometbft/cometbft/crypto"
	k1 "github.com/cometbft/cometbft/crypto/secp256k1"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/codec/address"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/runtime"
	simtestutil "github.com/cosmos/cosmos-sdk/testutil/sims"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	skeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"
	"github.com/cosmos/cosmos-sdk/x/staking/testutil"
	stypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/suite"

	evmenginetypes "github.com/piplabs/story/client/x/evmengine/types"
	"github.com/piplabs/story/client/x/evmstaking/keeper"
	"github.com/piplabs/story/client/x/evmstaking/module"
	estestutil "github.com/piplabs/story/client/x/evmstaking/testutil"
	"github.com/piplabs/story/client/x/evmstaking/types"
	minttypes "github.com/piplabs/story/client/x/mint/types"
	"github.com/piplabs/story/contracts/bindings"
	"github.com/piplabs/story/lib/errors"
	"github.com/piplabs/story/lib/ethclient"
	"github.com/piplabs/story/lib/k1util"

	"go.uber.org/mock/gomock"
)

var (
	PKs = simtestutil.CreateTestPubKeys(3)
)

func createAddresses(count int) ([]crypto.PubKey, []sdk.AccAddress, []sdk.ValAddress) {
	var pubKeys []crypto.PubKey
	var accAddrs []sdk.AccAddress
	var valAddrs []sdk.ValAddress
	for range count {
		pubKey := k1.GenPrivKey().PubKey()
		evmAddr, _ := k1util.CosmosPubkeyToEVMAddress(pubKey.Bytes())
		accAddr := sdk.AccAddress(pubKey.Address().Bytes())
		valAddr := sdk.ValAddress(evmAddr.Bytes())
		pubKeys = append(pubKeys, pubKey)
		accAddrs = append(accAddrs, accAddr)
		valAddrs = append(valAddrs, valAddr)
	}

	return pubKeys, accAddrs, valAddrs
}

type TestSuite struct {
	suite.Suite

	Ctx sdk.Context

	AccountKeeper authkeeper.AccountKeeper
	// AccountKeeper    *estestutil.MockAccountKeeper
	BankKeeper       bankkeeper.BaseKeeper
	DistrKeeper      *estestutil.MockDistributionKeeper
	StakingKeeper    *skeeper.Keeper
	SlashingKeeper   *estestutil.MockSlashingKeeper
	EVMStakingKeeper *keeper.Keeper
	queryClient      types.QueryClient

	encCfg moduletestutil.TestEncodingConfig
}

func (s *TestSuite) SetupTest() {
	s.encCfg = moduletestutil.MakeTestEncodingConfig(module.AppModuleBasic{})
	authKey := storetypes.NewKVStoreKey(authtypes.StoreKey)
	bankKey := storetypes.NewKVStoreKey(banktypes.StoreKey)
	evmstakingKey := storetypes.NewKVStoreKey(types.StoreKey)
	stakingKey := storetypes.NewKVStoreKey(stypes.StoreKey)

	authStoreService := runtime.NewKVStoreService(authKey)
	bankStoreService := runtime.NewKVStoreService(bankKey)
	storeService := runtime.NewKVStoreService(evmstakingKey)
	stakingStoreService := runtime.NewKVStoreService(stakingKey)

	db := dbm.NewMemDB()
	cms := store.NewCommitMultiStore(db, log.NewNopLogger(), metrics.NewNoOpMetrics())
	cms.MountStoreWithDB(authKey, storetypes.StoreTypeIAVL, db)
	cms.MountStoreWithDB(bankKey, storetypes.StoreTypeIAVL, db)
	cms.MountStoreWithDB(evmstakingKey, storetypes.StoreTypeIAVL, db)
	cms.MountStoreWithDB(stakingKey, storetypes.StoreTypeIAVL, db)
	err := cms.LoadLatestVersion()
	s.Require().NoError(err)

	s.Ctx = sdk.NewContext(cms, cmtproto.Header{Time: time.Now()}, false, log.NewNopLogger())

	interfaceRegistry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(interfaceRegistry)
	legacyAmino := codec.NewLegacyAmino()
	authtypes.RegisterLegacyAminoCodec(legacyAmino)
	authtypes.RegisterInterfaces(interfaceRegistry)
	banktypes.RegisterLegacyAminoCodec(legacyAmino)
	banktypes.RegisterInterfaces(interfaceRegistry)
	stypes.RegisterLegacyAminoCodec(legacyAmino)
	stypes.RegisterInterfaces(interfaceRegistry)
	marshaler := codec.NewProtoCodec(interfaceRegistry)

	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("story", "storypub")
	cfg.SetBech32PrefixForValidator("storyvaloper", "storyvaloperpub")
	cfg.SetBech32PrefixForConsensusNode("storyvalcons", "storyvalconspub")

	// gomock initializations
	ctrl := gomock.NewController(s.T())

	maccPerms := map[string][]string{
		authtypes.FeeCollectorName: nil,
		minttypes.ModuleName:       {authtypes.Minter},
		distrtypes.ModuleName:      nil,
		stypes.BondedPoolName:      {authtypes.Burner, authtypes.Staking},
		stypes.NotBondedPoolName:   {authtypes.Burner, authtypes.Staking},
		types.ModuleName:           {authtypes.Burner, authtypes.Minter},
		govtypes.ModuleName:        {authtypes.Burner},
	}

	accountKeeper := authkeeper.NewAccountKeeper(
		marshaler,
		authStoreService,
		authtypes.ProtoBaseAccount,
		maccPerms,
		address.NewBech32Codec(sdk.GetConfig().GetBech32AccountAddrPrefix()),
		"story",
		authtypes.NewModuleAddress(authtypes.ModuleName).String(),
	)
	s.AccountKeeper = accountKeeper

	blockedAddrs := make(map[string]bool)
	blockedAddrs[authtypes.NewModuleAddress(authtypes.FeeCollectorName).String()] = true
	blockedAddrs[authtypes.NewModuleAddress(authtypes.ModuleName).String()] = true
	blockedAddrs[authtypes.NewModuleAddress(distrtypes.ModuleName).String()] = true
	blockedAddrs[authtypes.NewModuleAddress(stypes.BondedPoolName).String()] = true
	blockedAddrs[authtypes.NewModuleAddress(stypes.NotBondedPoolName).String()] = true
	blockedAddrs[authtypes.NewModuleAddress(types.ModuleName).String()] = true

	bankKeeper := bankkeeper.NewBaseKeeper(
		marshaler,
		bankStoreService,
		accountKeeper,
		blockedAddrs,
		authtypes.NewModuleAddress(banktypes.ModuleName).String(),
		log.NewNopLogger(),
	)
	s.BankKeeper = bankKeeper

	distrKeeper := estestutil.NewMockDistributionKeeper(ctrl)
	s.DistrKeeper = distrKeeper
	slashingKeeper := estestutil.NewMockSlashingKeeper(ctrl)
	s.SlashingKeeper = slashingKeeper

	// staking keeper
	stakingKeeper := skeeper.NewKeeper(
		marshaler,
		stakingStoreService,
		accountKeeper,
		bankKeeper,
		authtypes.NewModuleAddress(stypes.ModuleName).String(),
		address.NewBech32Codec("storyvaloper"),
		address.NewBech32Codec("storyvalcons"),
	)
	s.StakingKeeper = stakingKeeper
	s.Require().NoError(s.StakingKeeper.SetParams(s.Ctx, stypes.DefaultParams()))

	// emvstaking keeper
	ethCl, err := ethclient.NewEngineMock(evmstakingKey)
	s.Require().NoError(err)
	evmstakingKeeper := keeper.NewKeeper(
		marshaler,
		storeService,
		accountKeeper,
		bankKeeper,
		slashingKeeper,
		stakingKeeper,
		distrKeeper,
		authtypes.NewModuleAddress(types.ModuleName).String(),
		ethCl,
		address.NewBech32Codec("storyvaloper"),
	)
	s.Require().NoError(evmstakingKeeper.SetParams(s.Ctx, types.DefaultParams()))
	s.EVMStakingKeeper = evmstakingKeeper
	queryHelper := baseapp.NewQueryServerTestHelper(s.Ctx, s.encCfg.InterfaceRegistry)
	types.RegisterQueryServer(queryHelper, evmstakingKeeper)
	s.queryClient = types.NewQueryClient(queryHelper)
}

func (s *TestSuite) TestLogger() {
	require := s.Require()
	logger := keeper.Logger(s.Ctx)
	require.NotNil(logger)
}

func (s *TestSuite) TestGetAuthority() {
	require := s.Require()
	require.Equal(authtypes.NewModuleAddress(types.ModuleName).String(), s.EVMStakingKeeper.GetAuthority())
}

func (s *TestSuite) TestValidatorAddressCodec() {
	require := s.Require()
	keeper := s.EVMStakingKeeper
	require.NotNil(keeper.ValidatorAddressCodec())
	_, err := keeper.ValidatorAddressCodec().StringToBytes("storyvaloper1hmjw3pvkjtndpg8wqppwdn8udd835qpaa6r6y0")
	require.NoError(err)
}

func cmpToEVM(cmpPubKey []byte) common.Address {
	evmAddr, err := keeper.CmpPubKeyToEVMAddress(cmpPubKey)
	if err != nil {
		panic(err)
	}

	return evmAddr
}

func (s *TestSuite) TestProcessStakingEvents() {
	require := s.Require()
	ctx, evmstakingKeeper, stakingKeeper := s.Ctx, s.EVMStakingKeeper, s.StakingKeeper
	// slashingKeeper := s.SlashingKeeper
	// create addresses
	pubKeys, addrs, valAddrs := createAddresses(3)
	// delegator info
	delAddr := addrs[0]
	delPubKey := pubKeys[0]
	delEvmAddr := common.BytesToAddress(pubKeys[0].Address().Bytes())
	// left padding the address to 32 bytes
	var evmAddrBytes [32]byte
	delEvmAddrBytes := delEvmAddr.Bytes()
	startIndex := len(evmAddrBytes) - len(delEvmAddrBytes)
	copy(evmAddrBytes[startIndex:], delEvmAddrBytes)
	// validator info
	// valAddr1 := valAddrs[1]
	valPubKey1 := pubKeys[1]
	// valAddr2 := valAddrs[2]
	// valPubKey2 := pubKeys[2]
	// self delegation amount
	// valTokens := stakingKeeper.TokensFromConsensusPower(ctx, 10)
	// abis
	stakingAbi, err := bindings.IPTokenStakingMetaData.GetAbi()
	require.NoError(err)
	// dummy hash
	dummyHash := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	// delegation amount
	delCoin := sdk.NewCoin(sdk.DefaultBondDenom, sdkmath.NewInt(100))
	// gwei multiplication for evm input
	gwei, exp := big.NewInt(10), big.NewInt(9)
	gwei.Exp(gwei, exp, nil)
	delAmtGwei := new(big.Int).Mul(gwei, new(big.Int).SetUint64(delCoin.Amount.Uint64()))

	tcs := []struct {
		name           string
		evmEvents      func() ([]*evmenginetypes.EVMEvent, error)
		setup          func(c context.Context)
		expectedError  string
		stateCheck     func(c context.Context)
		postStateCheck func(c context.Context)
	}{
		{
			name: "fail: invalid evm event",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				logs := []ethtypes.Log{{Topics: []common.Hash{types.SetWithdrawalAddress.ID, dummyHash}, TxHash: dummyHash}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}
				evmEvents[0].Address = nil

				return evmEvents, nil
			},
			expectedError: "verify log [BUG]",
		},
		// INVALID LOGS but PASS Cases because currently we are handling it as a continued
		{
			name: "pass(continue): invalid SetWithdrawalEvent log",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				logs := []ethtypes.Log{{Topics: []common.Hash{types.SetWithdrawalAddress.ID, dummyHash}, TxHash: dummyHash}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
		},
		{
			name: "pass(continue): invalid CreateValidatorEvent log",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				logs := []ethtypes.Log{{Topics: []common.Hash{types.CreateValidatorEvent.ID, dummyHash}, TxHash: dummyHash}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
		},
		{
			name: "pass(continue): invalid DepositEvent log",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				logs := []ethtypes.Log{{Topics: []common.Hash{types.DepositEvent.ID, dummyHash}, TxHash: dummyHash}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
		},
		{
			name: "pass(continue): invalid RedelegateEvent log",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				logs := []ethtypes.Log{{Topics: []common.Hash{types.RedelegateEvent.ID, dummyHash}, TxHash: dummyHash}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
		},
		{
			name: "pass(continue): invalid WithdrawEvent log",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				logs := []ethtypes.Log{{Topics: []common.Hash{types.WithdrawEvent.ID, dummyHash}, TxHash: dummyHash}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
		},
		{
			name: "pass(continue): invalid UnjailEvent log",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				logs := []ethtypes.Log{{Topics: []common.Hash{types.UnjailEvent.ID, dummyHash, dummyHash}, TxHash: dummyHash}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
		},
		// FAIL TO PROCESS but PASS Cases because currently we are handling it as a continued.
		// Only basic failure cases are validated. Various failure and success scenarios that may occur during the actual process
		// are tested separately with unit tests in the files where each processing logic is defined.
		{
			name: "pass(continue): fail to process CreateValidatorEvent - corrupted pubkey",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				data, err := stakingAbi.Events["CreateValidator"].Inputs.NonIndexed().Pack(
					createCorruptedPubKey(delPubKey.Bytes()),
					"moniker",
					delAmtGwei,
					uint32(1000),
					uint32(5000),
					uint32(500),
					uint8(0),
					cmpToEVM(delPubKey.Bytes()),
					[]byte{},
				)
				require.NoError(err)
				logs := []ethtypes.Log{{Topics: []common.Hash{types.CreateValidatorEvent.ID}, Data: data, TxHash: dummyHash}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
		},
		{
			name: "pass(continue): fail to process UnjailEvent - invalid validator pubkey",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				invalidValPubKey := valPubKey1.Bytes()[1:]
				data, err := stakingAbi.Events["Unjail"].Inputs.NonIndexed().Pack(
					cmpToEVM(delPubKey.Bytes()),
					invalidValPubKey,
					[]byte{},
				)
				require.NoError(err)
				logs := []ethtypes.Log{{Topics: []common.Hash{types.UnjailEvent.ID, common.BytesToHash(delEvmAddr.Bytes())}, Data: data}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
		},
		// SUCCESS Cases.
		{
			name: "pass: process SetWithdrawalAddressEvent",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				data, err := stakingAbi.Events["SetWithdrawalAddress"].Inputs.NonIndexed().Pack(
					common.Address(delAddr),
					evmAddrBytes,
				)
				require.NoError(err)
				logs := []ethtypes.Log{{Topics: []common.Hash{types.SetWithdrawalAddress.ID}, Data: data}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
			stateCheck: func(c context.Context) {
				_, err := evmstakingKeeper.DelegatorWithdrawAddress.Get(c, delPubKey.Address().String())
				require.ErrorContains(err, "not found")
			},
			postStateCheck: func(c context.Context) {
				evmDelAddr, err := evmstakingKeeper.DelegatorWithdrawAddress.Get(c, delAddr.String())
				require.NoError(err)
				require.Equal(delEvmAddr.String(), evmDelAddr)
			},
		},
		{
			name: "pass: process CreateValidatorEvent",
			evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
				data, err := stakingAbi.Events["CreateValidator"].Inputs.NonIndexed().Pack(
					delPubKey.Bytes(),
					"moniker",
					delAmtGwei,
					uint32(1000),
					uint32(5000),
					uint32(500),
					uint8(0),
					common.Address(delAddr),
					[]byte{},
				)
				require.NoError(err)
				logs := []ethtypes.Log{{Topics: []common.Hash{types.CreateValidatorEvent.ID}, Data: data}}
				evmEvents, err := ethLogsToEvmEvents(logs)
				if err != nil {
					return nil, err
				}

				return evmEvents, nil
			},
			setup: func(c context.Context) {
				// accountKeeper.EXPECT().HasAccount(c, delAddr).Return(true)
				// bankKeeper.EXPECT().MintCoins(c, types.ModuleName, sdk.NewCoins(delCoin))
				// bankKeeper.EXPECT().SendCoinsFromModuleToAccount(c, types.ModuleName, delAddr, sdk.NewCoins(delCoin))
				// bankKeeper.EXPECT().DelegateCoinsFromAccountToModule(c, delAddr, stypes.NotBondedPoolName, sdk.NewCoins(delCoin))
			},
			stateCheck: func(c context.Context) {
				_, err := stakingKeeper.GetValidator(c, valAddrs[0])
				require.ErrorContains(err, "validator does not exist")
			},
			postStateCheck: func(c context.Context) {
				newVal, err := stakingKeeper.GetValidator(c, valAddrs[0])
				require.NoError(err)
				require.Equal(valAddrs[0].String(), newVal.OperatorAddress)
				require.Equal("moniker", newVal.Description.GetMoniker())
				require.Equal(sdkmath.NewInt(100), newVal.Tokens)
				require.Equal("0.100000000000000000", newVal.Commission.Rate.String())
				require.Equal("0.500000000000000000", newVal.Commission.MaxRate.String())
				require.Equal("0.050000000000000000", newVal.Commission.MaxChangeRate.String())
			},
		},
		/*
			{
				name: "pass: process CreateValidatorEvent",
				evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
					data, err := stakingAbi.Events["CreateValidator"].Inputs.NonIndexed().Pack(
						uncompressedDelPubKeyBytes,
						"moniker",
						delAmtGwei,
						uint32(1000),
						uint32(5000),
						uint32(500),
						uint8(0),
						cmpToEVM(delPubKey.Bytes()),
						[]byte{},
					)
					require.NoError(err)
					logs := []ethtypes.Log{{Topics: []common.Hash{types.CreateValidatorEvent.ID}, Data: data}}
					evmEvents, err := ethLogsToEvmEvents(logs)
					if err != nil {
						return nil, err
					}

					return evmEvents, nil
				},
				setup: func(c context.Context) {
					accountKeeper.EXPECT().HasAccount(c, delAddr).Return(true)
					bankKeeper.EXPECT().MintCoins(c, types.ModuleName, sdk.NewCoins(delCoin))
					bankKeeper.EXPECT().SendCoinsFromModuleToAccount(c, types.ModuleName, delAddr, sdk.NewCoins(delCoin))
					bankKeeper.EXPECT().DelegateCoinsFromAccountToModule(c, delAddr, stypes.NotBondedPoolName, sdk.NewCoins(delCoin))
				},
				stateCheck: func(c context.Context) {
					_, err := stakingKeeper.GetValidator(c, sdk.ValAddress(delAddr))
					require.ErrorContains(err, "validator does not exist")
				},
				postStateCheck: func(c context.Context) {
					newVal, err := stakingKeeper.GetValidator(c, sdk.ValAddress(delAddr))
					require.NoError(err)
					require.Equal(sdk.ValAddress(delAddr).String(), newVal.OperatorAddress)
					require.Equal("moniker", newVal.Description.GetMoniker())
					require.Equal(sdkmath.NewInt(100), newVal.Tokens)
					require.Equal("0.100000000000000000", newVal.Commission.Rate.String())
					require.Equal("0.500000000000000000", newVal.Commission.MaxRate.String())
					require.Equal("0.050000000000000000", newVal.Commission.MaxChangeRate.String())
				},
			},
						{
							name: "pass: process DepositEvent",
							evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
								data, err := stakingAbi.Events["Deposit"].Inputs.NonIndexed().Pack(
									uncompressedDelPubKeyBytes,
									delPubKey.Bytes(),
									valPubKey1.Bytes(),
									delAmtGwei,
								)
								require.NoError(err)
								logs := []ethtypes.Log{{Topics: []common.Hash{types.DepositEvent.ID}, Data: data}}
								evmEvents, err := ethLogsToEvmEvents(logs)
								if err != nil {
									return nil, err
								}

								return evmEvents, nil
							},
							setup: func(c context.Context) {
								s.setupValidatorAndDelegation(c, valPubKey1, delPubKey, valAddr1, delAddr, valTokens)
								accountKeeper.EXPECT().HasAccount(c, delAddr).Return(true)
								bankKeeper.EXPECT().MintCoins(c, types.ModuleName, sdk.NewCoins(delCoin))
								bankKeeper.EXPECT().SendCoinsFromModuleToAccount(c, types.ModuleName, delAddr, sdk.NewCoins(delCoin))
								bankKeeper.EXPECT().DelegateCoinsFromAccountToModule(c, delAddr, stypes.BondedPoolName, sdk.NewCoins(delCoin))
							},
							stateCheck: func(c context.Context) {
								delegation, err := stakingKeeper.GetDelegation(c, delAddr, valAddr1)
								require.NoError(err)
								require.Equal(stakingKeeper.TokensFromConsensusPower(c, 100).ToLegacyDec(), delegation.GetShares())
							},
							postStateCheck: func(c context.Context) {
								delegation, err := stakingKeeper.GetDelegation(c, delAddr, valAddr1)
								require.NoError(err)
								require.Equal(
									stakingKeeper.TokensFromConsensusPower(c, 100).ToLegacyDec().Add(delCoin.Amount.ToLegacyDec()),
									delegation.GetShares(),
								)
							},
						},
					{
						name: "pass: process RedelegateEvent",
						evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
							data, err := stakingAbi.Events["Redelegate"].Inputs.NonIndexed().Pack(
								delPubKey.Bytes(),
								valPubKey1.Bytes(),
								valPubKey2.Bytes(),
								delAmtGwei,
							)
							require.NoError(err)
							logs := []ethtypes.Log{{Topics: []common.Hash{types.RedelegateEvent.ID}, Data: data}}
							evmEvents, err := ethLogsToEvmEvents(logs)
							if err != nil {
								return nil, err
							}

							return evmEvents, nil
						},
						setup: func(c context.Context) {
							s.setupValidatorAndDelegation(c, valPubKey1, delPubKey, valAddr1, delAddr, valTokens)
							s.setupValidatorAndDelegation(c, valPubKey2, delPubKey, valAddr2, delAddr, valTokens)
						},
						stateCheck: func(c context.Context) {
							_, err = stakingKeeper.GetRedelegation(c, delAddr, valAddr1, valAddr2)
							require.ErrorContains(err, "no redelegation found")
						},
						postStateCheck: func(c context.Context) {
							redelegation, err := stakingKeeper.GetRedelegation(c, delAddr, valAddr1, valAddr2)
							require.NoError(err)
							require.Equal(
								delCoin.Amount.Uint64(),
								redelegation.Entries[0].InitialBalance.Uint64(),
							)
						},
					},
				{
					name: "pass: process WithdrawEvent",
					evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
						data, err := stakingAbi.Events["Withdraw"].Inputs.NonIndexed().Pack(
							delPubKey.Bytes(),
							valPubKey1.Bytes(),
							delAmtGwei,
						)
						require.NoError(err)
						logs := []ethtypes.Log{{Topics: []common.Hash{types.WithdrawEvent.ID}, Data: data}}
						evmEvents, err := ethLogsToEvmEvents(logs)
						if err != nil {
							return nil, err
						}

						return evmEvents, nil
					},
					setup: func(c context.Context) {
						s.setupValidatorAndDelegation(c, valPubKey1, delPubKey, valAddr1, delAddr, valTokens)
						accountKeeper.EXPECT().HasAccount(c, delAddr).Return(true)
						bankKeeper.EXPECT().SendCoinsFromModuleToModule(c, stypes.BondedPoolName, stypes.NotBondedPoolName, gomock.Any())
					},
					stateCheck: func(c context.Context) {
						_, err = stakingKeeper.GetUnbondingDelegation(c, delAddr, valAddr1)
						require.ErrorContains(err, "no unbonding delegation found")
					},
					postStateCheck: func(c context.Context) {
						ubd, err := stakingKeeper.GetUnbondingDelegation(c, delAddr, valAddr1)
						require.NoError(err)
						require.Equal(
							delCoin.Amount.Uint64(),
							ubd.Entries[0].InitialBalance.Uint64(),
						)
					},
				},
				{
					name: "pass: process UnjailEvent",
					evmEvents: func() ([]*evmenginetypes.EVMEvent, error) {
						data, err := stakingAbi.Events["Unjail"].Inputs.NonIndexed().Pack(valPubKey1.Bytes())
						require.NoError(err)
						logs := []ethtypes.Log{{Topics: []common.Hash{types.UnjailEvent.ID, common.BytesToHash(delEvmAddr.Bytes())}, Data: data}}
						evmEvents, err := ethLogsToEvmEvents(logs)
						if err != nil {
							return nil, err
						}

						return evmEvents, nil
					},
					setup: func(c context.Context) {
						// We just check if the unjail function is called.
						// Because we are using the mock slashing keeper, we cannot check the staking module's state.
						slashingKeeper.EXPECT().Unjail(c, valAddr1).Return(nil)
					},
				},
		*/
	}

	for _, tc := range tcs {
		s.Run(tc.name, func() {
			cachedCtx, _ := ctx.CacheContext()
			if tc.setup != nil {
				tc.setup(cachedCtx)
			}
			evmLogs, err := tc.evmEvents()
			require.NoError(err)
			if tc.stateCheck != nil {
				tc.stateCheck(cachedCtx)
			}
			err = evmstakingKeeper.ProcessStakingEvents(cachedCtx, 1, evmLogs)
			if tc.expectedError != "" {
				require.Error(err)
				require.Contains(err.Error(), tc.expectedError)
			} else {
				require.NoError(err)
				if tc.postStateCheck != nil {
					tc.postStateCheck(cachedCtx)
				}
			}
		})
	}
}

func TestTestSuite(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(TestSuite))
}

// setupValidatorAndDelegation creates a validator and delegation for testing.
func (s *TestSuite) setupValidatorAndDelegation(ctx context.Context, valPubKey, delPubKey crypto.PubKey, valAddr sdk.ValAddress, delAddr sdk.AccAddress, valTokens sdkmath.Int) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	require := s.Require()
	accountKeeper, bankKeeper, stakingKeeper, evmstakingKeeper := s.AccountKeeper, s.BankKeeper, s.StakingKeeper, s.EVMStakingKeeper

	notBondedPool := stakingKeeper.GetNotBondedPool(ctx)
	accountKeeper.SetModuleAccount(ctx, notBondedPool)

	initCoins := sdk.NewCoins(sdk.NewCoin(sdk.DefaultBondDenom, valTokens))
	require.NoError(bankKeeper.MintCoins(ctx, types.ModuleName, initCoins))
	require.NoError(bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, delAddr, initCoins))
	require.NoError(bankKeeper.MintCoins(ctx, types.ModuleName, initCoins))
	require.NoError(bankKeeper.SendCoinsFromModuleToModule(ctx, types.ModuleName, notBondedPool.GetName(), initCoins))

	// Convert public key to cosmos format
	valCosmosPubKey, err := k1util.PubKeyToCosmos(valPubKey)
	require.NoError(err)

	// Create and update validator
	val := testutil.NewValidator(s.T(), valAddr, valCosmosPubKey)
	validator, issuedShares, _ := val.AddTokensFromDel(valTokens, sdkmath.LegacyOneDec())
	require.Equal(valTokens, issuedShares.RoundInt())

	// bankKeeper.EXPECT().SendCoinsFromModuleToModule(gomock.Any(), stypes.NotBondedPoolName, stypes.BondedPoolName, gomock.Any()).Times(2)
	// _ = skeeper.TestingUpdateValidator(stakingKeeper, sdkCtx, validator, true)

	// Create/Get period delegation
	periodDelegation, err := stakingKeeper.GetOrCreatePeriodDelegation(ctx, delAddr, valAddr, stypes.FlexiblePeriodDelegationID, stypes.DefaultFlexiblePeriodType, time.Now())
	require.NoError(err)

	// Create and set delegation
	delAmt := stakingKeeper.TokensFromConsensusPower(ctx, 100).ToLegacyDec()
	_, _, err = stakingKeeper.Delegate(
		ctx, delAddr, delAmt.RoundInt(), stypes.Unbonded, val, false,
		periodDelegation.PeriodDelegationId, periodDelegation.PeriodType, periodDelegation.EndTime,
	)
	require.NoError(err)

	validator.DelegatorShares = validator.DelegatorShares.Add(delAmt)
	validator.DelegatorRewardsShares = validator.DelegatorRewardsShares.Add(delAmt)
	require.NoError(bankKeeper.MintCoins(ctx, types.ModuleName, initCoins))
	require.NoError(bankKeeper.SendCoinsFromModuleToModule(ctx, types.ModuleName, notBondedPool.GetName(), initCoins))
	_ = skeeper.TestingUpdateValidator(stakingKeeper, sdkCtx, validator, true)

	// Map delegator to EVM address
	delEvmAddr, err := k1util.CosmosPubkeyToEVMAddress(delPubKey.Bytes())
	require.NoError(err)
	require.NoError(evmstakingKeeper.DelegatorWithdrawAddress.Set(ctx, delAddr.String(), delEvmAddr.String()))
	require.NoError(evmstakingKeeper.DelegatorRewardAddress.Set(ctx, delAddr.String(), delEvmAddr.String()))
	// require.NoError(evmstakingKeeper.DelegatorOperatorAddress.Set(ctx, delAddr.String(), delEvmAddr.String()))

	// Ensure delegation is set correctly
	delegation, err := stakingKeeper.GetDelegation(ctx, delAddr, valAddr)
	require.NoError(err)
	require.Equal(delAmt, delegation.GetShares())
}

func createCorruptedPubKey(pubKey []byte) []byte {
	corruptedPubKey := append([]byte(nil), pubKey...)
	// corruptedPubKey[0] = 0x03
	corruptedPubKey[1] = 0xFF

	return corruptedPubKey
}

// ethLogsToEvmEvents converts Ethereum logs to a slice of EVM events.
func ethLogsToEvmEvents(logs []ethtypes.Log) ([]*evmenginetypes.EVMEvent, error) {
	events := make([]*evmenginetypes.EVMEvent, 0, len(logs))
	for _, l := range logs {
		topics := make([][]byte, 0, len(l.Topics))
		for _, t := range l.Topics {
			topics = append(topics, t.Bytes())
		}
		events = append(events, &evmenginetypes.EVMEvent{
			Address: l.Address.Bytes(),
			Topics:  topics,
			Data:    l.Data,
			TxHash:  l.TxHash.Bytes(),
		})
	}

	for _, log := range events {
		if err := log.Verify(); err != nil {
			return nil, errors.Wrap(err, "verify log")
		}
	}

	return events, nil
}
