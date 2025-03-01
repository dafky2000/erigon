package state22

import (
	"context"
	"math/big"
	"sync"

	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/consensus/misc"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/systemcontracts"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync"
	"github.com/ledgerwatch/log/v3"
)

type Worker22 struct {
	lock         sync.Locker
	chainDb      kv.RoDB
	chainTx      kv.Tx
	db           kv.RoDB
	tx           kv.Tx
	wg           *sync.WaitGroup
	rs           *state.State22
	blockReader  services.FullBlockReader
	allSnapshots *snapshotsync.RoSnapshots
	stateWriter  *state.StateWriter22
	stateReader  *state.StateReader22
	getHeader    func(hash common.Hash, number uint64) *types.Header
	ctx          context.Context
	engine       consensus.Engine
	txNums       []uint64
	chainConfig  *params.ChainConfig
	logger       log.Logger
	genesis      *core.Genesis
	resultCh     chan *state.TxTask
	epoch        EpochReader
	chain        ChainReader
	isPoSA       bool
	posa         consensus.PoSA
}

func NewWorker22(lock sync.Locker, db kv.RoDB, chainDb kv.RoDB, wg *sync.WaitGroup, rs *state.State22,
	blockReader services.FullBlockReader, allSnapshots *snapshotsync.RoSnapshots,
	txNums []uint64, chainConfig *params.ChainConfig, logger log.Logger, genesis *core.Genesis,
	resultCh chan *state.TxTask, engine consensus.Engine,
) *Worker22 {
	ctx := context.Background()
	w := &Worker22{
		lock:         lock,
		db:           db,
		chainDb:      chainDb,
		wg:           wg,
		rs:           rs,
		blockReader:  blockReader,
		allSnapshots: allSnapshots,
		ctx:          ctx,
		stateWriter:  state.NewStateWriter22(rs),
		stateReader:  state.NewStateReader22(rs),
		txNums:       txNums,
		chainConfig:  chainConfig,
		logger:       logger,
		genesis:      genesis,
		resultCh:     resultCh,
		engine:       engine,
		getHeader: func(hash common.Hash, number uint64) *types.Header {
			h, err := blockReader.Header(ctx, nil, hash, number)
			if err != nil {
				panic(err)
			}
			return h
		},
	}
	w.posa, w.isPoSA = engine.(consensus.PoSA)
	return w
}

func (rw *Worker22) Tx() kv.Tx { return rw.tx }
func (rw *Worker22) ResetTx(tx kv.Tx, chainTx kv.Tx) {
	if rw.tx != nil {
		rw.tx.Rollback()
		rw.tx = nil
	}
	if rw.chainTx != nil {
		rw.chainTx.Rollback()
		rw.chainTx = nil
	}
	if tx != nil {
		rw.tx = tx
		rw.stateReader.SetTx(rw.tx)
	}
	if chainTx != nil {
		rw.chainTx = chainTx
		rw.epoch = EpochReader{tx: rw.chainTx}
		rw.chain = ChainReader{config: rw.chainConfig, tx: rw.chainTx, blockReader: rw.blockReader}
	}
}

func (rw *Worker22) Run() {
	defer rw.wg.Done()
	for txTask, ok := rw.rs.Schedule(); ok; txTask, ok = rw.rs.Schedule() {
		rw.RunTxTask(txTask)
		rw.resultCh <- txTask // Needs to have outside of the lock
	}
}

func (rw *Worker22) RunTxTask(txTask *state.TxTask) {
	rw.lock.Lock()
	defer rw.lock.Unlock()
	if rw.tx == nil {
		var err error
		if rw.tx, err = rw.db.BeginRo(rw.ctx); err != nil {
			panic(err)
		}
		rw.stateReader.SetTx(rw.tx)
	}
	if rw.chainTx == nil {
		var err error
		if rw.chainTx, err = rw.chainDb.BeginRo(rw.ctx); err != nil {
			panic(err)
		}
		rw.epoch = EpochReader{tx: rw.chainTx}
		rw.chain = ChainReader{config: rw.chainConfig, tx: rw.chainTx, blockReader: rw.blockReader}
	}
	txTask.Error = nil
	rw.stateReader.SetTxNum(txTask.TxNum)
	rw.stateWriter.SetTxNum(txTask.TxNum)
	rw.stateReader.ResetReadSet()
	rw.stateWriter.ResetWriteSet()
	ibs := state.New(rw.stateReader)
	rules := txTask.Rules
	daoForkTx := rw.chainConfig.DAOForkSupport && rw.chainConfig.DAOForkBlock != nil && rw.chainConfig.DAOForkBlock.Uint64() == txTask.BlockNum && txTask.TxIndex == -1
	var err error
	if txTask.BlockNum == 0 && txTask.TxIndex == -1 {
		//fmt.Printf("txNum=%d, blockNum=%d, Genesis\n", txTask.TxNum, txTask.BlockNum)
		// Genesis block
		_, ibs, err = rw.genesis.ToBlock()
		if err != nil {
			panic(err)
		}
		// For Genesis, rules should be empty, so that empty accounts can be included
		rules = &params.Rules{}
	} else if daoForkTx {
		//fmt.Printf("txNum=%d, blockNum=%d, DAO fork\n", txTask.TxNum, txTask.BlockNum)
		misc.ApplyDAOHardFork(ibs)
		ibs.SoftFinalise()
	} else if txTask.TxIndex == -1 {
		// Block initialisation
		//fmt.Printf("txNum=%d, blockNum=%d, initialisation of the block\n", txTask.TxNum, txTask.BlockNum)
		if rw.isPoSA {
			systemcontracts.UpgradeBuildInSystemContract(rw.chainConfig, txTask.Header.Number, ibs)
		}
		syscall := func(contract common.Address, data []byte) ([]byte, error) {
			return core.SysCallContract(contract, data, *rw.chainConfig, ibs, txTask.Header, rw.engine)
		}
		rw.engine.Initialize(rw.chainConfig, rw.chain, rw.epoch, txTask.Header, txTask.Block.Transactions(), txTask.Block.Uncles(), syscall)
	} else if txTask.Final {
		if txTask.BlockNum > 0 {
			//fmt.Printf("txNum=%d, blockNum=%d, finalisation of the block\n", txTask.TxNum, txTask.BlockNum)
			// End of block transaction in a block
			syscall := func(contract common.Address, data []byte) ([]byte, error) {
				return core.SysCallContract(contract, data, *rw.chainConfig, ibs, txTask.Header, rw.engine)
			}
			if _, _, err := rw.engine.Finalize(rw.chainConfig, txTask.Header, ibs, txTask.Block.Transactions(), txTask.Block.Uncles(), nil /* receipts */, rw.epoch, rw.chain, syscall); err != nil {
				//fmt.Printf("error=%v\n", err)
				txTask.Error = err
			} else {
				txTask.TraceTos = map[common.Address]struct{}{}
				txTask.TraceTos[txTask.Block.Coinbase()] = struct{}{}
				for _, uncle := range txTask.Block.Uncles() {
					txTask.TraceTos[uncle.Coinbase] = struct{}{}
				}
			}
		}
	} else {
		//fmt.Printf("txNum=%d, blockNum=%d, txIndex=%d\n", txTask.TxNum, txTask.BlockNum, txTask.TxIndex)
		if rw.isPoSA {
			if isSystemTx, err := rw.posa.IsSystemTransaction(txTask.Tx, txTask.Header); err != nil {
				panic(err)
			} else if isSystemTx {
				//fmt.Printf("System tx\n")
				return
			}
		}
		txHash := txTask.Tx.Hash()
		gp := new(core.GasPool).AddGas(txTask.Tx.GetGas())
		ct := NewCallTracer()
		vmConfig := vm.Config{Debug: true, Tracer: ct, SkipAnalysis: core.SkipAnalysis(rw.chainConfig, txTask.BlockNum)}
		contractHasTEVM := func(contractHash common.Hash) (bool, error) { return false, nil }
		ibs.Prepare(txHash, txTask.BlockHash, txTask.TxIndex)
		getHashFn := core.GetHashFn(txTask.Header, rw.getHeader)
		blockContext := core.NewEVMBlockContext(txTask.Header, getHashFn, rw.engine, nil /* author */, contractHasTEVM)
		msg, err := txTask.Tx.AsMessage(*types.MakeSigner(rw.chainConfig, txTask.BlockNum), txTask.Header.BaseFee, txTask.Rules)
		if err != nil {
			panic(err)
		}
		txContext := core.NewEVMTxContext(msg)
		vmenv := vm.NewEVM(blockContext, txContext, ibs, rw.chainConfig, vmConfig)
		if _, err = core.ApplyMessage(vmenv, msg, gp, true /* refunds */, false /* gasBailout */); err != nil {
			txTask.Error = err
			//fmt.Printf("error=%v\n", err)
		} else {
			// Update the state with pending changes
			ibs.SoftFinalise()
			txTask.Logs = ibs.GetLogs(txHash)
			txTask.TraceFroms = ct.froms
			txTask.TraceTos = ct.tos
		}
	}
	// Prepare read set, write set and balanceIncrease set and send for serialisation
	if txTask.Error == nil {
		txTask.BalanceIncreaseSet = ibs.BalanceIncreaseSet()
		//for addr, bal := range txTask.BalanceIncreaseSet {
		//	fmt.Printf("[%x]=>[%d]\n", addr, &bal)
		//}
		if err = ibs.MakeWriteSet(rules, rw.stateWriter); err != nil {
			panic(err)
		}
		txTask.ReadLists = rw.stateReader.ReadSet()
		txTask.WriteLists = rw.stateWriter.WriteSet()
		txTask.AccountPrevs, txTask.AccountDels, txTask.StoragePrevs, txTask.CodePrevs = rw.stateWriter.PrevAndDels()
		size := (20 + 32) * len(txTask.BalanceIncreaseSet)
		for _, list := range txTask.ReadLists {
			for _, b := range list.Keys {
				size += len(b)
			}
			for _, b := range list.Vals {
				size += len(b)
			}
		}
		for _, list := range txTask.WriteLists {
			for _, b := range list.Keys {
				size += len(b)
			}
			for _, b := range list.Vals {
				size += len(b)
			}
		}
		txTask.ResultsSize = int64(size)
	}
}

type ChainReader struct {
	config      *params.ChainConfig
	tx          kv.Tx
	blockReader services.FullBlockReader
}

func NewChainReader(config *params.ChainConfig, tx kv.Tx, blockReader services.FullBlockReader) ChainReader {
	return ChainReader{config: config, tx: tx, blockReader: blockReader}
}

func (cr ChainReader) Config() *params.ChainConfig  { return cr.config }
func (cr ChainReader) CurrentHeader() *types.Header { panic("") }
func (cr ChainReader) GetHeader(hash common.Hash, number uint64) *types.Header {
	if cr.blockReader != nil {
		h, _ := cr.blockReader.Header(context.Background(), cr.tx, hash, number)
		return h
	}
	return rawdb.ReadHeader(cr.tx, hash, number)
}
func (cr ChainReader) GetHeaderByNumber(number uint64) *types.Header {
	if cr.blockReader != nil {
		h, _ := cr.blockReader.HeaderByNumber(context.Background(), cr.tx, number)
		return h
	}
	return rawdb.ReadHeaderByNumber(cr.tx, number)

}
func (cr ChainReader) GetHeaderByHash(hash common.Hash) *types.Header {
	if cr.blockReader != nil {
		number := rawdb.ReadHeaderNumber(cr.tx, hash)
		if number == nil {
			return nil
		}
		return cr.GetHeader(hash, *number)
	}
	h, _ := rawdb.ReadHeaderByHash(cr.tx, hash)
	return h
}
func (cr ChainReader) GetTd(hash common.Hash, number uint64) *big.Int {
	td, err := rawdb.ReadTd(cr.tx, hash, number)
	if err != nil {
		log.Error("ReadTd failed", "err", err)
		return nil
	}
	return td
}

type EpochReader struct {
	tx kv.Tx
}

func NewEpochReader(tx kv.Tx) EpochReader { return EpochReader{tx: tx} }

func (cr EpochReader) GetEpoch(hash common.Hash, number uint64) ([]byte, error) {
	return rawdb.ReadEpoch(cr.tx, number, hash)
}
func (cr EpochReader) PutEpoch(hash common.Hash, number uint64, proof []byte) error {
	panic("")
}
func (cr EpochReader) GetPendingEpoch(hash common.Hash, number uint64) ([]byte, error) {
	return rawdb.ReadPendingEpoch(cr.tx, number, hash)
}
func (cr EpochReader) PutPendingEpoch(hash common.Hash, number uint64, proof []byte) error {
	panic("")
}
func (cr EpochReader) FindBeforeOrEqualNumber(number uint64) (blockNum uint64, blockHash common.Hash, transitionProof []byte, err error) {
	return rawdb.FindEpochBeforeOrEqualNumber(cr.tx, number)
}

func NewWorkersPool(lock sync.Locker, db kv.RoDB, chainDb kv.RoDB, wg *sync.WaitGroup, rs *state.State22,
	blockReader services.FullBlockReader, allSnapshots *snapshotsync.RoSnapshots,
	txNums []uint64, chainConfig *params.ChainConfig, logger log.Logger, genesis *core.Genesis,
	engine consensus.Engine,
	workerCount int) (reconWorkers []*Worker22, resultCh chan *state.TxTask, clear func()) {
	queueSize := workerCount * 4
	reconWorkers = make([]*Worker22, workerCount)
	resultCh = make(chan *state.TxTask, queueSize)
	for i := 0; i < workerCount; i++ {
		reconWorkers[i] = NewWorker22(lock, db, chainDb, wg, rs, blockReader, allSnapshots, txNums, chainConfig, logger, genesis, resultCh, engine)
	}
	clear = func() {
		for i := 0; i < workerCount; i++ {
			reconWorkers[i].ResetTx(nil, nil)
		}
	}
	if workerCount > 1 {
		wg.Add(workerCount)
		for i := 0; i < workerCount; i++ {
			go reconWorkers[i].Run()
		}
	}
	return reconWorkers, resultCh, clear
}
