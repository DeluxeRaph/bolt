// Copyright 2018 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"crypto/ecdsa"
	"math/big"
	mrnd "math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chainbound/shardmap"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

const (
	// testCode is the testing contract binary code which will initialises some
	// variables in constructor
	testCode = "0x60806040527fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff0060005534801561003457600080fd5b5060fc806100436000396000f3fe6080604052348015600f57600080fd5b506004361060325760003560e01c80630c4dae8814603757806398a213cf146053575b600080fd5b603d607e565b6040518082815260200191505060405180910390f35b607c60048036036020811015606757600080fd5b81019080803590602001909291905050506084565b005b60005481565b806000819055507fe9e44f9f7da8c559de847a3232b57364adc0354f15a2cd8dc636d54396f9587a6000546040518082815260200191505060405180910390a15056fea265627a7a723058208ae31d9424f2d0bc2a3da1a5dd659db2d71ec322a17db8f87e19e209e3a1ff4a64736f6c634300050a0032"

	// testGas is the gas required for contract deployment.
	testGas = 144109
)

var (
	// Test chain configurations
	testTxPoolConfig  legacypool.Config
	ethashChainConfig *params.ChainConfig
	cliqueChainConfig *params.ChainConfig

	// Test accounts
	testBankKey, _  = crypto.GenerateKey()
	testBankAddress = crypto.PubkeyToAddress(testBankKey.PublicKey)
	testBankFunds   = big.NewInt(1000000000000000000)

	testAddress1Key, _ = crypto.GenerateKey()
	testAddress1       = crypto.PubkeyToAddress(testAddress1Key.PublicKey)
	testAddress2Key, _ = crypto.GenerateKey()
	testAddress2       = crypto.PubkeyToAddress(testAddress2Key.PublicKey)
	testAddress3Key, _ = crypto.GenerateKey()
	testAddress3       = crypto.PubkeyToAddress(testAddress3Key.PublicKey)

	testUserKey, _  = crypto.GenerateKey()
	testUserAddress = crypto.PubkeyToAddress(testUserKey.PublicKey)

	// Test transactions
	pendingTxs []*types.Transaction
	newTxs     []*types.Transaction

	// Test testConstraintsCache
	testConstraintsCache = new(shardmap.FIFOMap[uint64, types.HashToConstraintDecoded])

	testConfig = &Config{
		Recommit: time.Second,
		GasCeil:  params.GenesisGasLimit,
	}

	defaultGenesisAlloc = types.GenesisAlloc{testBankAddress: {Balance: testBankFunds}}
)

const pendingTxsLen = 50

func init() {
	testTxPoolConfig = legacypool.DefaultConfig
	testTxPoolConfig.Journal = ""
	ethashChainConfig = new(params.ChainConfig)
	*ethashChainConfig = *params.TestChainConfig
	cliqueChainConfig = new(params.ChainConfig)
	*cliqueChainConfig = *params.TestChainConfig
	cliqueChainConfig.Clique = &params.CliqueConfig{
		Period: 10,
		Epoch:  30000,
	}

	signer := types.LatestSigner(params.TestChainConfig)
	for i := 0; i < pendingTxsLen; i++ {
		tx1 := types.MustSignNewTx(testBankKey, signer, &types.AccessListTx{
			ChainID:  params.TestChainConfig.ChainID,
			Nonce:    uint64(i),
			To:       &testUserAddress,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: big.NewInt(params.InitialBaseFee),
		})

		// Add some constraints every 3 txs, and every 6 add an index
		if i%3 == 0 {
			idx := new(uint64)
			if i%2 == 0 {
				*idx = uint64(i)
			} else {
				idx = nil
			}
			constraints := make(map[common.Hash]*types.ConstraintDecoded)
			constraints[tx1.Hash()] = &types.ConstraintDecoded{Index: idx, Tx: tx1}
			// FIXME: slot 0 is probably not correct for these tests
			testConstraintsCache.Put(0, constraints)
		}

		pendingTxs = append(pendingTxs, tx1)
	}

	tx2 := types.MustSignNewTx(testBankKey, signer, &types.LegacyTx{
		Nonce:    1,
		To:       &testUserAddress,
		Value:    big.NewInt(1000),
		Gas:      params.TxGas,
		GasPrice: big.NewInt(params.InitialBaseFee),
	})
	newTxs = append(newTxs, tx2)
}

// testWorkerBackend implements worker.Backend interfaces and wraps all information needed during the testing.
type testWorkerBackend struct {
	db      ethdb.Database
	txPool  *txpool.TxPool
	chain   *core.BlockChain
	genesis *core.Genesis
}

func newTestWorkerBackend(t *testing.T, chainConfig *params.ChainConfig, engine consensus.Engine, db ethdb.Database, alloc types.GenesisAlloc, n int, gasLimit uint64) *testWorkerBackend {
	if alloc == nil {
		alloc = defaultGenesisAlloc
	}
	gspec := &core.Genesis{
		Config:   chainConfig,
		GasLimit: gasLimit,
		Alloc:    alloc,
	}
	switch e := engine.(type) {
	case *clique.Clique:
		gspec.ExtraData = make([]byte, 32+common.AddressLength+crypto.SignatureLength)
		copy(gspec.ExtraData[32:32+common.AddressLength], testBankAddress.Bytes())
		e.Authorize(testBankAddress, func(account accounts.Account, s string, data []byte) ([]byte, error) {
			return crypto.Sign(crypto.Keccak256(data), testBankKey)
		})
	case *ethash.Ethash:
	default:
		t.Fatalf("unexpected consensus engine type: %T", engine)
	}
	chain, err := core.NewBlockChain(db, &core.CacheConfig{TrieDirtyDisabled: true}, gspec, nil, engine, vm.Config{}, nil, nil)
	if err != nil {
		t.Fatalf("core.NewBlockChain failed: %v", err)
	}
	pool := legacypool.New(testTxPoolConfig, chain)
	txpool, _ := txpool.New(testTxPoolConfig.PriceLimit, chain, []txpool.SubPool{pool})

	return &testWorkerBackend{
		db:      db,
		chain:   chain,
		txPool:  txpool,
		genesis: gspec,
	}
}

func (b *testWorkerBackend) BlockChain() *core.BlockChain { return b.chain }
func (b *testWorkerBackend) TxPool() *txpool.TxPool       { return b.txPool }

func (b *testWorkerBackend) newRandomTx(creation bool, to common.Address, amt int64, key *ecdsa.PrivateKey, additionalGasLimit uint64, gasPrice *big.Int) *types.Transaction {
	var tx *types.Transaction
	if creation {
		tx, _ = types.SignTx(types.NewContractCreation(b.txPool.Nonce(crypto.PubkeyToAddress(key.PublicKey)), big.NewInt(0), testGas, gasPrice, common.FromHex(testCode)), types.HomesteadSigner{}, key)
	} else {
		tx, _ = types.SignTx(types.NewTransaction(b.txPool.Nonce(crypto.PubkeyToAddress(key.PublicKey)), to, big.NewInt(amt), params.TxGas+additionalGasLimit, gasPrice, nil), types.HomesteadSigner{}, key)
	}
	return tx
}

func newTestWorker(t *testing.T, chainConfig *params.ChainConfig, engine consensus.Engine, db ethdb.Database, alloc types.GenesisAlloc, blocks int) (*worker, *testWorkerBackend) {
	const GasLimit = 1_000_000_000_000_000_000
	backend := newTestWorkerBackend(t, chainConfig, engine, db, alloc, blocks, GasLimit)
	backend.txPool.Add(pendingTxs, true, false, false)
	w := newWorker(testConfig, chainConfig, engine, backend, new(event.TypeMux), nil, false, &flashbotsData{
		isFlashbots: testConfig.AlgoType != ALGO_MEV_GETH,
		queue:       nil,
		bundleCache: NewBundleCache(),
		algoType:    testConfig.AlgoType,
	})
	if testConfig.BuilderTxSigningKey == nil {
		w.setEtherbase(testBankAddress)
	}

	return w, backend
}

func TestGenerateAndImportBlock(t *testing.T) {
	t.Parallel()
	var (
		db     = rawdb.NewMemoryDatabase()
		config = *params.AllCliqueProtocolChanges
	)
	config.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
	engine := clique.New(config.Clique, db)

	w, b := newTestWorker(t, &config, engine, db, nil, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	chain, _ := core.NewBlockChain(rawdb.NewMemoryDatabase(), nil, b.genesis, nil, engine, vm.Config{}, nil, nil)
	defer chain.Stop()

	// Ignore empty commit here for less noise.
	w.skipSealHook = func(task *task) bool {
		return len(task.receipts) == 0
	}

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	// Start mining!
	w.start()

	for i := 0; i < 5; i++ {
		b.txPool.Add([]*types.Transaction{b.newRandomTx(true, testUserAddress, 0, testBankKey, 0, big.NewInt(10*params.InitialBaseFee))}, true, false, false)
		b.txPool.Add([]*types.Transaction{b.newRandomTx(false, testUserAddress, 1000, testBankKey, 0, big.NewInt(10*params.InitialBaseFee))}, true, false, false)

		select {
		case ev := <-sub.Chan():
			block := ev.Data.(core.NewMinedBlockEvent).Block
			if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
				t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
			}
		case <-time.After(3 * time.Second): // Worker needs 1s to include new changes.
			t.Fatalf("timeout")
		}
	}
}

func TestEmptyWorkEthash(t *testing.T) {
	t.Parallel()
	testEmptyWork(t, ethashChainConfig, ethash.NewFaker())
}

func TestEmptyWorkClique(t *testing.T) {
	t.Parallel()
	testEmptyWork(t, cliqueChainConfig, clique.New(cliqueChainConfig.Clique, rawdb.NewMemoryDatabase()))
}

func testEmptyWork(t *testing.T, chainConfig *params.ChainConfig, engine consensus.Engine) {
	defer engine.Close()

	w, _ := newTestWorker(t, chainConfig, engine, rawdb.NewMemoryDatabase(), nil, 0)
	defer w.close()

	taskCh := make(chan struct{}, pendingTxsLen*2)
	checkEqual := func(t *testing.T, task *task) {
		// The work should contain 1 tx
		receiptLen, balance := pendingTxsLen, uint256.NewInt(50_000)
		if len(task.receipts) != receiptLen {
			t.Fatalf("receipt number mismatch: have %d, want %d", len(task.receipts), receiptLen)
		}
		if task.state.GetBalance(testUserAddress).Cmp(balance) != 0 {
			t.Fatalf("account balance mismatch: have %d, want %d", task.state.GetBalance(testUserAddress), balance)
		}
	}
	w.newTaskHook = func(task *task) {
		if task.block.NumberU64() == 1 {
			checkEqual(t, task)
			taskCh <- struct{}{}
		}
	}
	w.skipSealHook = func(task *task) bool { return true }
	w.fullTaskHook = func() {
		time.Sleep(100 * time.Millisecond)
	}
	w.start() // Start mining!
	select {
	case <-taskCh:
	case <-time.NewTimer(3 * time.Second).C:
		t.Error("new task timeout")
	}
}

func TestAdjustIntervalEthash(t *testing.T) {
	t.Parallel()
	testAdjustInterval(t, ethashChainConfig, ethash.NewFaker())
}

func TestAdjustIntervalClique(t *testing.T) {
	t.Parallel()
	testAdjustInterval(t, cliqueChainConfig, clique.New(cliqueChainConfig.Clique, rawdb.NewMemoryDatabase()))
}

func testAdjustInterval(t *testing.T, chainConfig *params.ChainConfig, engine consensus.Engine) {
	defer engine.Close()

	w, _ := newTestWorker(t, chainConfig, engine, rawdb.NewMemoryDatabase(), nil, 0)
	defer w.close()

	w.skipSealHook = func(task *task) bool {
		return true
	}
	w.fullTaskHook = func() {
		time.Sleep(100 * time.Millisecond)
	}
	var (
		progress = make(chan struct{}, 10)
		result   = make([]float64, 0, 10)
		index    = 0
		start    atomic.Bool
	)
	w.resubmitHook = func(minInterval, recommitInterval time.Duration) {
		// Short circuit if interval checking hasn't started.
		if !start.Load() {
			return
		}
		var wantMinInterval, wantRecommitInterval time.Duration

		switch index {
		case 0:
			wantMinInterval, wantRecommitInterval = 3*time.Second, 3*time.Second
		case 1:
			origin := float64(3 * time.Second.Nanoseconds())
			estimate := origin*(1-intervalAdjustRatio) + intervalAdjustRatio*(origin/0.8+intervalAdjustBias)
			wantMinInterval, wantRecommitInterval = 3*time.Second, time.Duration(estimate)*time.Nanosecond
		case 2:
			estimate := result[index-1]
			min := float64(3 * time.Second.Nanoseconds())
			estimate = estimate*(1-intervalAdjustRatio) + intervalAdjustRatio*(min-intervalAdjustBias)
			wantMinInterval, wantRecommitInterval = 3*time.Second, time.Duration(estimate)*time.Nanosecond
		case 3:
			wantMinInterval, wantRecommitInterval = time.Second, time.Second
		}

		// Check interval
		if minInterval != wantMinInterval {
			t.Errorf("resubmit min interval mismatch: have %v, want %v ", minInterval, wantMinInterval)
		}
		if recommitInterval != wantRecommitInterval {
			t.Errorf("resubmit interval mismatch: have %v, want %v", recommitInterval, wantRecommitInterval)
		}
		result = append(result, float64(recommitInterval.Nanoseconds()))
		index += 1
		progress <- struct{}{}
	}
	w.start()

	time.Sleep(time.Second) // Ensure two tasks have been submitted due to start opt
	start.Store(true)

	w.setRecommitInterval(3 * time.Second)
	select {
	case <-progress:
	case <-time.NewTimer(time.Second).C:
		t.Error("interval reset timeout")
	}

	w.resubmitAdjustCh <- &intervalAdjust{inc: true, ratio: 0.8}
	select {
	case <-progress:
	case <-time.NewTimer(time.Second).C:
		t.Error("interval reset timeout")
	}

	w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	select {
	case <-progress:
	case <-time.NewTimer(time.Second).C:
		t.Error("interval reset timeout")
	}

	w.setRecommitInterval(500 * time.Millisecond)
	select {
	case <-progress:
	case <-time.NewTimer(time.Second).C:
		t.Error("interval reset timeout")
	}
}

func TestGetSealingWorkEthash(t *testing.T) {
	t.Parallel()
	testGetSealingWork(t, ethashChainConfig, ethash.NewFaker(), nil)
}

func TestGetSealingWorkClique(t *testing.T) {
	t.Parallel()
	testGetSealingWork(t, cliqueChainConfig, clique.New(cliqueChainConfig.Clique, rawdb.NewMemoryDatabase()), nil)
}

func TestGetSealingWorkPostMerge(t *testing.T) {
	t.Parallel()
	local := new(params.ChainConfig)
	*local = *ethashChainConfig
	local.TerminalTotalDifficulty = big.NewInt(0)
	testGetSealingWork(t, local, ethash.NewFaker(), nil)
}

// TestGetSealingWorkWithConstraints tests the getSealingWork function with constraints.
// This is the main test for the modified block building algorithm. Unfortunately
// is not easy to make an end to end test where the constraints are pulled from the relay.
//
// A suggestion is to walk through the executing code with a debugger to further inspect the algorithm.
//
// However, if you want to check that functionality see `builder_test.go`
func TestGetSealingWorkWithConstraints(t *testing.T) {
	// t.Parallel()
	local := new(params.ChainConfig)
	*local = *ethashChainConfig
	local.TerminalTotalDifficulty = big.NewInt(0)
	testGetSealingWork(t, local, ethash.NewFaker(), testConstraintsCache)
}

func testGetSealingWork(t *testing.T, chainConfig *params.ChainConfig, engine consensus.Engine, constraintsCache *shardmap.FIFOMap[uint64, types.HashToConstraintDecoded]) {
	defer engine.Close()
	w, b := newTestWorker(t, chainConfig, engine, rawdb.NewMemoryDatabase(), nil, 0)
	defer w.close()

	w.setExtra([]byte{0x01, 0x02})

	w.skipSealHook = func(task *task) bool {
		return true
	}
	w.fullTaskHook = func() {
		time.Sleep(100 * time.Millisecond)
	}
	timestamp := uint64(time.Now().Unix())
	assertBlock := func(block *types.Block, number uint64, coinbase common.Address, random common.Hash, noExtra bool) {
		if block.Time() != timestamp {
			// Sometime the timestamp will be mutated if the timestamp
			// is even smaller than parent block's. It's OK.
			t.Logf("Invalid timestamp, want %d, get %d", timestamp, block.Time())
		}
		_, isClique := engine.(*clique.Clique)
		if !isClique {
			if len(block.Extra()) != 2 {
				t.Error("Unexpected extra field")
			}
			//if block.Coinbase() != coinbase {
			//	t.Errorf("Unexpected coinbase got %x want %x", block.Coinbase(), coinbase)
			//}
		} else {
			if block.Coinbase() != (common.Address{}) {
				t.Error("Unexpected coinbase")
			}
		}
		if !isClique {
			if block.MixDigest() != random {
				t.Error("Unexpected mix digest")
			}
		}
		if block.Nonce() != 0 {
			t.Error("Unexpected block nonce")
		}
		if block.NumberU64() != number {
			t.Errorf("Mismatched block number, want %d got %d", number, block.NumberU64())
		}
	}
	cases := []struct {
		parent       common.Hash
		coinbase     common.Address
		random       common.Hash
		expectNumber uint64
		expectErr    bool
	}{
		{
			b.chain.Genesis().Hash(),
			common.HexToAddress("0xdeadbeef"),
			common.HexToHash("0xcafebabe"),
			uint64(1),
			false,
		},
		{
			b.chain.CurrentBlock().Hash(),
			common.HexToAddress("0xdeadbeef"),
			common.HexToHash("0xcafebabe"),
			b.chain.CurrentBlock().Number.Uint64() + 1,
			false,
		},
		{
			b.chain.CurrentBlock().Hash(),
			common.Address{},
			common.HexToHash("0xcafebabe"),
			b.chain.CurrentBlock().Number.Uint64() + 1,
			false,
		},
		{
			b.chain.CurrentBlock().Hash(),
			common.Address{},
			common.Hash{},
			b.chain.CurrentBlock().Number.Uint64() + 1,
			false,
		},
		{
			common.HexToHash("0xdeadbeef"),
			common.HexToAddress("0xdeadbeef"),
			common.HexToHash("0xcafebabe"),
			0,
			true,
		},
	}

	// This API should work even when the automatic sealing is not enabled
	for _, c := range cases {
		r := w.getSealingBlock(&generateParams{
			parentHash:       c.parent,
			timestamp:        timestamp,
			coinbase:         c.coinbase,
			random:           c.random,
			withdrawals:      nil,
			beaconRoot:       nil,
			noTxs:            false,
			forceTime:        true,
			onBlock:          nil,
			constraintsCache: constraintsCache,
		})
		if c.expectErr {
			if r.err == nil {
				t.Error("Expect error but get nil")
			}
		} else {
			if r.err != nil {
				t.Errorf("Unexpected error %v", r.err)
			}
			assertBlock(r.block, c.expectNumber, c.coinbase, c.random, true)
		}
	}

	// This API should work even when the automatic sealing is enabled
	w.start()
	for _, c := range cases {
		r := w.getSealingBlock(&generateParams{
			parentHash:  c.parent,
			timestamp:   timestamp,
			coinbase:    c.coinbase,
			random:      c.random,
			withdrawals: nil,
			beaconRoot:  nil,
			noTxs:       false,
			forceTime:   true,
			onBlock:     nil,
		})
		if c.expectErr {
			if r.err == nil {
				t.Error("Expect error but get nil")
			}
		} else {
			if r.err != nil {
				t.Errorf("Unexpected error %v", r.err)
			}
			assertBlock(r.block, c.expectNumber, c.coinbase, c.random, false)
		}
	}
}

func TestSimulateBundles(t *testing.T) {
	w, _ := newTestWorker(t, ethashChainConfig, ethash.NewFaker(), rawdb.NewMemoryDatabase(), nil, 0)
	defer w.close()

	env, err := w.prepareWork(&generateParams{gasLimit: 30000000})
	if err != nil {
		t.Fatalf("Failed to prepare work: %s", err)
	}

	signTx := func(nonce uint64) *types.Transaction {
		tx, err := types.SignTx(types.NewTransaction(nonce, testUserAddress, big.NewInt(1000), params.TxGas, env.header.BaseFee, nil), types.HomesteadSigner{}, testBankKey)
		if err != nil {
			t.Fatalf("Failed to sign tx")
		}
		return tx
	}

	bundle1 := types.MevBundle{Txs: types.Transactions{signTx(0)}, Hash: common.HexToHash("0x01")}
	// this bundle will fail
	bundle2 := types.MevBundle{Txs: types.Transactions{signTx(1)}, Hash: common.HexToHash("0x02")}
	bundle3 := types.MevBundle{Txs: types.Transactions{signTx(0)}, Hash: common.HexToHash("0x03")}

	simBundles, _, err := w.simulateBundles(env, []types.MevBundle{bundle1, bundle2, bundle3}, nil, nil)
	require.NoError(t, err)

	if len(simBundles) != 2 {
		t.Fatalf("Incorrect amount of sim bundles")
	}

	for _, simBundle := range simBundles {
		if simBundle.OriginalBundle.Hash == common.HexToHash("0x02") {
			t.Fatalf("bundle2 should fail")
		}
	}

	// simulate 2 times to check cache
	simBundles, _, err = w.simulateBundles(env, []types.MevBundle{bundle1, bundle2, bundle3}, nil, nil)
	require.NoError(t, err)

	if len(simBundles) != 2 {
		t.Fatalf("Incorrect amount of sim bundles(cache)")
	}

	for _, simBundle := range simBundles {
		if simBundle.OriginalBundle.Hash == common.HexToHash("0x02") {
			t.Fatalf("bundle2 should fail(cache)")
		}
	}
}

func testBundles(t *testing.T) {
	// TODO: test cancellations
	db := rawdb.NewMemoryDatabase()
	chainConfig := params.AllEthashProtocolChanges
	engine := ethash.NewFaker()

	chainConfig.LondonBlock = big.NewInt(0)

	genesisAlloc := types.GenesisAlloc{testBankAddress: {Balance: testBankFunds}}

	nExtraKeys := 5
	extraKeys := make([]*ecdsa.PrivateKey, nExtraKeys)
	for i := 0; i < nExtraKeys; i++ {
		pk, _ := crypto.GenerateKey()
		address := crypto.PubkeyToAddress(pk.PublicKey)
		extraKeys[i] = pk
		genesisAlloc[address] = types.Account{Balance: testBankFunds}
	}

	nSearchers := 5
	searcherPrivateKeys := make([]*ecdsa.PrivateKey, nSearchers)
	for i := 0; i < nSearchers; i++ {
		pk, _ := crypto.GenerateKey()
		address := crypto.PubkeyToAddress(pk.PublicKey)
		searcherPrivateKeys[i] = pk
		genesisAlloc[address] = types.Account{Balance: testBankFunds}
	}

	for _, address := range []common.Address{testAddress1, testAddress2, testAddress3} {
		genesisAlloc[address] = types.Account{Balance: testBankFunds}
	}

	w, b := newTestWorker(t, chainConfig, engine, db, nil, 0)
	w.setEtherbase(crypto.PubkeyToAddress(testConfig.BuilderTxSigningKey.PublicKey))
	defer w.close()

	// Ignore empty commit here for less noise.
	w.skipSealHook = func(task *task) bool {
		return len(task.receipts) == 0
	}

	mrnd.New(mrnd.NewSource(10))

	for i := 0; i < 2; i++ {
		commonTxs := []*types.Transaction{
			b.newRandomTx(false, testBankAddress, 1e15, testAddress1Key, 0, big.NewInt(100*params.InitialBaseFee)),
			b.newRandomTx(false, testBankAddress, 1e15, testAddress2Key, 0, big.NewInt(110*params.InitialBaseFee)),
			b.newRandomTx(false, testBankAddress, 1e15, testAddress3Key, 0, big.NewInt(120*params.InitialBaseFee)),
		}

		searcherTxs := make([]*types.Transaction, len(searcherPrivateKeys)*2)
		for i, pk := range searcherPrivateKeys {
			searcherTxs[2*i] = b.newRandomTx(false, testBankAddress, 1, pk, 0, big.NewInt(150*params.InitialBaseFee))
			searcherTxs[2*i+1] = b.newRandomTx(false, testBankAddress, 1+1, pk, 0, big.NewInt(150*params.InitialBaseFee))
		}

		nBundles := 2 * len(searcherPrivateKeys)
		// two bundles per searcher, i and i+1
		bundles := make([]*types.MevBundle, nBundles)
		for i := 0; i < nBundles; i++ {
			bundles[i] = new(types.MevBundle)
			bundles[i].Txs = append(bundles[i].Txs, searcherTxs[i])
		}

		// common transactions in 10% of the bundles, randomly
		for i := 0; i < nBundles/10; i++ {
			randomCommonIndex := mrnd.Intn(len(commonTxs))
			randomBundleIndex := mrnd.Intn(nBundles)
			bundles[randomBundleIndex].Txs = append(bundles[randomBundleIndex].Txs, commonTxs[randomCommonIndex])
		}

		// additional lower profit transactions in 10% of the bundles, randomly
		for _, extraKey := range extraKeys {
			tx := b.newRandomTx(false, testBankAddress, 1, extraKey, 0, big.NewInt(20*params.InitialBaseFee))
			randomBundleIndex := mrnd.Intn(nBundles)
			bundles[randomBundleIndex].Txs = append(bundles[randomBundleIndex].Txs, tx)
		}

		blockNumber := big.NewInt(0).Add(w.chain.CurrentBlock().Number, big.NewInt(1))
		for _, bundle := range bundles {
			err := b.txPool.AddMevBundle(bundle.Txs, blockNumber, types.EmptyUUID, common.Address{}, 0, 0, nil)
			require.NoError(t, err)
		}

		r := w.getSealingBlock(&generateParams{
			parentHash:  w.chain.CurrentBlock().Hash(),
			timestamp:   w.chain.CurrentHeader().Time + 12,
			coinbase:    testUserAddress,
			random:      common.Hash{},
			withdrawals: nil,
			beaconRoot:  nil,
			noTxs:       false,
			onBlock:     nil,
		})
		require.NoError(t, r.err)

		state, err := w.chain.State()
		require.NoError(t, err)
		balancePre := state.GetBalance(testUserAddress)
		if _, err := w.chain.InsertChain([]*types.Block{r.block}); err != nil {
			t.Fatalf("failed to insert new mined block %d: %v", r.block.NumberU64(), err)
		}
		state, err = w.chain.StateAt(r.block.Root())
		require.NoError(t, err)
		balancePost := state.GetBalance(testUserAddress)
		t.Log("Balances", balancePre, balancePost)
	}
}
