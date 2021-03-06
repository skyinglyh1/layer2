/*
 * Copyright (C) 2018 The ontology Authors
 * This file is part of The ontology library.
 *
 * The ontology is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * The ontology is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Lesser General Public License for more details.
 *
 * You should have received a copy of the GNU Lesser General Public License
 * along with The ontology.  If not, see <http://www.gnu.org/licenses/>.
 */
//Storage of ledger
package ledgerstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/ontio/layer2/node/merkle"
	types2 "github.com/ontio/layer2/node/vm/neovm/types"
	"hash"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ontio/ontology-crypto/keypair"
	"github.com/ontio/layer2/node/common"
	"github.com/ontio/layer2/node/common/config"
	"github.com/ontio/layer2/node/common/log"
	"github.com/ontio/layer2/node/core/payload"
	"github.com/ontio/layer2/node/core/signature"
	"github.com/ontio/layer2/node/core/states"
	"github.com/ontio/layer2/node/core/store"
	scom "github.com/ontio/layer2/node/core/store/common"
	"github.com/ontio/layer2/node/core/store/overlaydb"
	"github.com/ontio/layer2/node/core/types"
	"github.com/ontio/layer2/node/errors"
	"github.com/ontio/layer2/node/events"
	"github.com/ontio/layer2/node/events/message"
	"github.com/ontio/layer2/node/smartcontract"
	"github.com/ontio/layer2/node/smartcontract/event"
	"github.com/ontio/layer2/node/smartcontract/service/neovm"
	sstate "github.com/ontio/layer2/node/smartcontract/states"
	"github.com/ontio/layer2/node/smartcontract/storage"
)

const (
	SYSTEM_VERSION          = byte(1)      //Version of ledger store
	HEADER_INDEX_BATCH_SIZE = uint32(2000) //Bath size of saving header index
)

var (
	//Storage save path.
	DBDirEvent          = "ledgerevent"
	DBDirBlock          = "block"
	DBDirState          = "states"
	MerkleTreeStorePath = "merkle_tree.db"
)

type PrexecuteParam struct {
	JitMode    bool
	WasmFactor uint64
	MinGas     bool
}

//LedgerStoreImp is main store struct fo ledger
type LedgerStoreImp struct {
	blockStore           *BlockStore                      //BlockStore for saving block & transaction data
	stateStore           *StateStore                      //StateStore for saving state data, like balance, smart contract execution result, and so on.
	eventStore           *EventStore                      //EventStore for saving log those gen after smart contract executed.
	layer2Store          *Layer2Store
	storedIndexCount     uint32                           //record the count of have saved block index
	currBlockHeight      uint32                           //Current block height
	currBlockHash        common.Uint256                   //Current block hash
	headerIndex          map[uint32]common.Uint256        //Header index, Mapping header height => block hash
	savingBlockSemaphore chan bool
	closing              bool
	lock                 sync.RWMutex
	stateHashCheckHeight uint32
}

//NewLedgerStore return LedgerStoreImp instance
func NewLedgerStore(dataDir string, stateHashHeight uint32) (*LedgerStoreImp, error) {
	ledgerStore := &LedgerStoreImp{
		headerIndex:          make(map[uint32]common.Uint256),
		savingBlockSemaphore: make(chan bool, 1),
		stateHashCheckHeight: stateHashHeight,
	}

	blockStore, err := NewBlockStore(fmt.Sprintf("%s%s%s", dataDir, string(os.PathSeparator), DBDirBlock), true)
	if err != nil {
		return nil, fmt.Errorf("NewBlockStore error %s", err)
	}
	ledgerStore.blockStore = blockStore

	layer2Store, err := NewLayer2Store(dataDir)
	if err != nil {
		return nil, fmt.Errorf("NewBlockStore error %s", err)
	}
	ledgerStore.layer2Store = layer2Store

	dbPath := fmt.Sprintf("%s%s%s", dataDir, string(os.PathSeparator), DBDirState)
	merklePath := fmt.Sprintf("%s%s%s", dataDir, string(os.PathSeparator), MerkleTreeStorePath)
	stateStore, err := NewStateStore(dbPath, merklePath, stateHashHeight)
	if err != nil {
		return nil, fmt.Errorf("NewStateStore error %s", err)
	}
	ledgerStore.stateStore = stateStore

	eventState, err := NewEventStore(fmt.Sprintf("%s%s%s", dataDir, string(os.PathSeparator), DBDirEvent))
	if err != nil {
		return nil, fmt.Errorf("NewEventStore error %s", err)
	}
	ledgerStore.eventStore = eventState

	return ledgerStore, nil
}

//InitLedgerStoreWithGenesisBlock init the ledger store with genesis block. It's the first operation after NewLedgerStore.
func (this *LedgerStoreImp) InitLedgerStoreWithGenesisBlock(genesisBlock *types.Block, defaultBookkeeper []keypair.PublicKey) error {
	hasInit, err := this.hasAlreadyInitGenesisBlock()
	if err != nil {
		return fmt.Errorf("hasAlreadyInit error %s", err)
	}
	if !hasInit {
		err = this.blockStore.ClearAll()
		if err != nil {
			return fmt.Errorf("blockStore.ClearAll error %s", err)
		}
		err = this.stateStore.ClearAll()
		if err != nil {
			return fmt.Errorf("stateStore.ClearAll error %s", err)
		}
		err = this.eventStore.ClearAll()
		if err != nil {
			return fmt.Errorf("eventStore.ClearAll error %s", err)
		}
		defaultBookkeeper = keypair.SortPublicKeys(defaultBookkeeper)
		bookkeeperState := &states.BookkeeperState{
			CurrBookkeeper: defaultBookkeeper,
			NextBookkeeper: defaultBookkeeper,
		}
		err = this.stateStore.SaveBookkeeperState(bookkeeperState)
		if err != nil {
			return fmt.Errorf("SaveBookkeeperState error %s", err)
		}

		result, err := this.executeBlock(genesisBlock)
		if err != nil {
			return err
		}
		err = this.submitBlock(genesisBlock, nil, result)
		if err != nil {
			return fmt.Errorf("save genesis block error %s", err)
		}
		err = this.initGenesisBlock()
		if err != nil {
			return fmt.Errorf("init error %s", err)
		}
		genHash := genesisBlock.Hash()
		log.Infof("GenesisBlock init success. GenesisBlock hash:%s\n", genHash.ToHexString())
	} else {
		genesisHash := genesisBlock.Hash()
		exist, err := this.blockStore.ContainBlock(genesisHash)
		if err != nil {
			return fmt.Errorf("HashBlockExist error %s", err)
		}
		if !exist {
			return fmt.Errorf("GenesisBlock arenot init correctly")
		}
		err = this.init()
		if err != nil {
			return fmt.Errorf("init error %s", err)
		}
	}
	return err
}

func (this *LedgerStoreImp) hasAlreadyInitGenesisBlock() (bool, error) {
	version, err := this.blockStore.GetVersion()
	if err != nil && err != scom.ErrNotFound {
		return false, fmt.Errorf("GetVersion error %s", err)
	}
	return version == SYSTEM_VERSION, nil
}

func (this *LedgerStoreImp) initGenesisBlock() error {
	return this.blockStore.SaveVersion(SYSTEM_VERSION)
}

func (this *LedgerStoreImp) init() error {
	err := this.loadCurrentBlock()
	if err != nil {
		return fmt.Errorf("loadCurrentBlock error %s", err)
	}
	err = this.loadHeaderIndexList()
	if err != nil {
		return fmt.Errorf("loadHeaderIndexList error %s", err)
	}
	err = this.recoverStore()
	if err != nil {
		return fmt.Errorf("recoverStore error %s", err)
	}
	return nil
}

func (this *LedgerStoreImp) loadCurrentBlock() error {
	currentBlockHash, currentBlockHeight, err := this.blockStore.GetCurrentBlock()
	if err != nil {
		return fmt.Errorf("LoadCurrentBlock error %s", err)
	}
	log.Infof("InitCurrentBlock currentBlockHash %s currentBlockHeight %d", currentBlockHash.ToHexString(), currentBlockHeight)
	this.currBlockHash = currentBlockHash
	this.currBlockHeight = currentBlockHeight
	return nil
}

func (this *LedgerStoreImp) loadHeaderIndexList() error {
	currBlockHeight := this.GetCurrentBlockHeight()
	headerIndex, err := this.blockStore.GetHeaderIndexList()
	if err != nil {
		return fmt.Errorf("LoadHeaderIndexList error %s", err)
	}
	storeIndexCount := uint32(len(headerIndex))
	this.headerIndex = headerIndex
	this.storedIndexCount = storeIndexCount

	for i := storeIndexCount; i <= currBlockHeight; i++ {
		height := i
		blockHash, err := this.blockStore.GetBlockHash(height)
		if err != nil {
			return fmt.Errorf("LoadBlockHash height %d error %s", height, err)
		}
		if blockHash == common.UINT256_EMPTY {
			return fmt.Errorf("LoadBlockHash height %d hash nil", height)
		}
		this.headerIndex[height] = blockHash
	}
	return nil
}

func (this *LedgerStoreImp) recoverStore() error {
	blockHeight := this.GetCurrentBlockHeight()

	_, stateHeight, err := this.stateStore.GetCurrentBlock()
	if err != nil {
		return fmt.Errorf("stateStore.GetCurrentBlock error %s", err)
	}
	for i := stateHeight; i < blockHeight; i++ {
		blockHash, err := this.blockStore.GetBlockHash(i)
		if err != nil {
			return fmt.Errorf("blockStore.GetBlockHash height:%d error:%s", i, err)
		}
		block, err := this.blockStore.GetBlock(blockHash)
		if err != nil {
			return fmt.Errorf("blockStore.GetBlock height:%d error:%s", i, err)
		}
		this.eventStore.NewBatch()
		this.stateStore.NewBatch()
		result, err := this.executeBlock(block)
		if err != nil {
			return err
		}
		err = this.saveBlockToStateStore(block, result)
		if err != nil {
			return fmt.Errorf("save to state store height:%d error:%s", i, err)
		}
		this.saveBlockToEventStore(block)
		err = this.eventStore.CommitTo()
		if err != nil {
			return fmt.Errorf("eventStore.CommitTo height:%d error %s", i, err)
		}
		err = this.stateStore.CommitTo()
		if err != nil {
			return fmt.Errorf("stateStore.CommitTo height:%d error %s", i, err)
		}
	}
	return nil
}

func (this *LedgerStoreImp) setHeaderIndex(height uint32, blockHash common.Uint256) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.headerIndex[height] = blockHash
}

func (this *LedgerStoreImp) getHeaderIndex(height uint32) common.Uint256 {
	this.lock.RLock()
	defer this.lock.RUnlock()
	blockHash, ok := this.headerIndex[height]
	if !ok {
		return common.Uint256{}
	}
	return blockHash
}

//GetCurrentHeaderHeight return the current header height.
//In block sync states, Header height is usually higher than block height that is has already committed to storage
func (this *LedgerStoreImp) GetCurrentHeaderHeight() uint32 {
	this.lock.RLock()
	defer this.lock.RUnlock()
	size := len(this.headerIndex)
	if size == 0 {
		return 0
	}
	return uint32(size) - 1
}

//GetCurrentHeaderHash return the current header hash. The current header means the latest header.
func (this *LedgerStoreImp) GetCurrentHeaderHash() common.Uint256 {
	this.lock.RLock()
	defer this.lock.RUnlock()
	size := len(this.headerIndex)
	if size == 0 {
		return common.Uint256{}
	}
	return this.headerIndex[uint32(size)-1]
}

func (this *LedgerStoreImp) setCurrentBlock(height uint32, blockHash common.Uint256) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.currBlockHash = blockHash
	this.currBlockHeight = height
	return
}

//GetCurrentBlock return the current block height, and block hash.
//Current block means the latest block in store.
func (this *LedgerStoreImp) GetCurrentBlock() (uint32, common.Uint256) {
	this.lock.RLock()
	defer this.lock.RUnlock()
	return this.currBlockHeight, this.currBlockHash
}

//GetCurrentBlockHash return the current block hash
func (this *LedgerStoreImp) GetCurrentBlockHash() common.Uint256 {
	this.lock.RLock()
	defer this.lock.RUnlock()
	return this.currBlockHash
}

//GetCurrentBlockHeight return the current block height
func (this *LedgerStoreImp) GetCurrentBlockHeight() uint32 {
	this.lock.RLock()
	defer this.lock.RUnlock()
	return this.currBlockHeight
}

func (this *LedgerStoreImp) verifyHeader(header *types.Header) (error) {
	if header.Height == 0 {
		return nil
	}
	var prevHeader *types.Header
	prevHeaderHash := header.PrevBlockHash
	prevHeader, err := this.GetHeaderByHash(prevHeaderHash)
	if err != nil && err != scom.ErrNotFound {
		return fmt.Errorf("get prev header error %s", err)
	}
	if prevHeader == nil {
		return fmt.Errorf("cannot find pre header by blockHash %s", prevHeaderHash.ToHexString())
	}

	if prevHeader.Height+1 != header.Height {
		return fmt.Errorf("block height is incorrect")
	}

	if prevHeader.Timestamp >= header.Timestamp {
		return fmt.Errorf("block timestamp is incorrect")
	}
	{
		address, err := types.AddressFromBookkeepers(header.Bookkeepers)
		if err != nil {
			return err
		}
		if prevHeader.NextBookkeeper != address {
			return fmt.Errorf("bookkeeper address error")
		}

		m := len(header.Bookkeepers) - (len(header.Bookkeepers)-1)/3
		hash := header.Hash()
		err = signature.VerifyMultiSignature(hash[:], header.Bookkeepers, m, header.SigData)
		if err != nil {
			return err
		}
	}
	return nil
}

func (this *LedgerStoreImp) verifyLayer2State(layer2State *types.Layer2State, bookkeepers []keypair.PublicKey) error {
	hash := layer2State.Hash()
	m := len(bookkeepers) - (len(bookkeepers)-1)/3
	err := signature.VerifyMultiSignature(hash[:], bookkeepers, m, layer2State.SigData)
	if err != nil {
		log.Errorf("VerifyMultiSignature of layer2 state:%s,heigh:%d", err, layer2State.Height)
		return err
	}
	return nil
}

func (this *LedgerStoreImp) GetStateMerkleRoot(height uint32) (common.Uint256, error) {
	return this.stateStore.GetStateMerkleRoot(height)
}

func (this *LedgerStoreImp) ExecuteBlock(block *types.Block) (result store.ExecuteResult, err error) {
	this.getSavingBlockLock()
	defer this.releaseSavingBlockLock()
	currBlockHeight := this.GetCurrentBlockHeight()
	blockHeight := block.Header.Height
	if blockHeight <= currBlockHeight {
		result.MerkleRoot, err = this.GetStateMerkleRoot(blockHeight)
		return
	}
	nextBlockHeight := currBlockHeight + 1
	if blockHeight != nextBlockHeight {
		err = fmt.Errorf("block height %d not equal next block height %d", blockHeight, nextBlockHeight)
		return
	}

	result, err = this.executeBlock(block)
	return
}

func (this *LedgerStoreImp) SubmitBlock(block *types.Block, layer2State *types.Layer2State, result store.ExecuteResult) error {
	this.getSavingBlockLock()
	defer this.releaseSavingBlockLock()
	if this.closing {
		return errors.NewErr("save block error: ledger is closing")
	}
	currBlockHeight := this.GetCurrentBlockHeight()
	blockHeight := block.Header.Height
	if blockHeight <= currBlockHeight {
		return nil
	}
	nextBlockHeight := currBlockHeight + 1
	if blockHeight != nextBlockHeight {
		return fmt.Errorf("block height %d not equal next block height %d", blockHeight, nextBlockHeight)
	}

	err := this.verifyHeader(block.Header)
	if err != nil {
		return fmt.Errorf("verifyHeader error %s", err)
	}
	if layer2State != nil {
		if layer2State.Height != nextBlockHeight {
			return fmt.Errorf("layer2 state msg height %d not equal next block height %d", nextBlockHeight, layer2State.Height)
		}
		if layer2State.Version != types.CURR_LAYER2_STATE_VERSION {
			return fmt.Errorf("error layer2 state msg version excepted:%d actual:%d", types.CURR_LAYER2_STATE_VERSION, layer2State.Version)
		}
		/*
		root, err := this.stateStore.GetLayer2StateRoot(ccMsg.Height)
		if err != nil {
			return fmt.Errorf("get layer2 state root fail:%s", err)
		}
		if root != ccMsg.StatesRoot {
			return fmt.Errorf("layer2 state root compare fail, expected:%x actual:%x", ccMsg.StatesRoot, root)
		}
		*/
		if err := this.verifyLayer2State(layer2State, block.Header.Bookkeepers); err != nil {
			return fmt.Errorf("verifyLayer2State error: %s", err)
		}
	}

	err = this.submitBlock(block, layer2State, result)
	if err != nil {
		return fmt.Errorf("saveBlock error %s", err)
	}
	return nil
}

func (this *LedgerStoreImp) saveBlockToBlockStore(block *types.Block) error {
	blockHash := block.Hash()
	blockHeight := block.Header.Height

	this.setHeaderIndex(blockHeight, blockHash)
	err := this.saveHeaderIndexList()
	if err != nil {
		return fmt.Errorf("saveHeaderIndexList error %s", err)
	}
	err = this.blockStore.SaveCurrentBlock(blockHeight, blockHash)
	if err != nil {
		return fmt.Errorf("SaveCurrentBlock error %s", err)
	}
	this.blockStore.SaveBlockHash(blockHeight, blockHash)
	err = this.blockStore.SaveBlock(block)
	if err != nil {
		return fmt.Errorf("SaveBlock height %d hash %s error %s", blockHeight, blockHash.ToHexString(), err)
	}
	return nil
}

func (this *LedgerStoreImp) executeBlock(block *types.Block) (result store.ExecuteResult, err error) {
	overlay := this.stateStore.NewOverlayDB()
	if block.Header.Height != 0 {
		config := &smartcontract.Config{
			Time:   block.Header.Timestamp,
			Height: block.Header.Height,
			Tx:     &types.Transaction{},
		}

		err = refreshGlobalParam(config, storage.NewCacheDB(this.stateStore.NewOverlayDB()), this)
		if err != nil {
			return
		}
	}
	gasTable := make(map[string]uint64)
	neovm.GAS_TABLE.Range(func(k, value interface{}) bool {
		key := k.(string)
		val := value.(uint64)
		gasTable[key] = val

		return true
	})

	cache := storage.NewCacheDB(overlay)
	for _, tx := range block.Transactions {
		cache.Reset()
		notify, e := this.handleTransaction(overlay, cache, gasTable, block, tx)
		if e != nil {
			err = e
			return
		}
		result.Notify = append(result.Notify, notify)
	}
	result.Hash = overlay.ChangeHash()
	result.WriteSet = overlay.GetWriteSet()
	if block.Header.Height < this.stateHashCheckHeight {
		result.MerkleRoot = common.UINT256_EMPTY
	} else if block.Header.Height == this.stateHashCheckHeight {
		res, e := calculateTotalStateHash(overlay)
		if e != nil {
			err = e
			return
		}

		result.MerkleRoot = res
		result.Hash = result.MerkleRoot
	} else {
		result.MerkleRoot = this.stateStore.GetStateMerkleRootWithNewHash(result.Hash)
	}
	result.UpdatedAccountStateRoot, result.UpdatedAccountState = this.calculateChangeStateRoot(cache)
	log.Infof("New state root: %s", result.UpdatedAccountStateRoot.ToHexString())
	return
}

type KeyState struct {
	Key      []byte
	Value    []byte
}

type KeyStateSlice []*KeyState
func (this KeyStateSlice) Len() int {
	return len(this)
}
func (this KeyStateSlice) Swap(i, j int) {
	key := this[i]
	this[i] = this[j]
	this[j] = key
}
func (this KeyStateSlice) Less(i, j int) bool {
	return bytes.Compare(this[i].Key, this[j].Key) > 0
}

func (this *LedgerStoreImp) isAccountUpdateStore(key, val []byte) bool {
	// [ST_STORAGE:ContractAddr:UserAddr] = 1 + 20 + 20
	if len(key) != 41 {
		return false
	}
	return true
}

func (this *LedgerStoreImp) calculateChangeStateRoot(cache *storage.CacheDB) (common.Uint256, []common.Uint256) {
	memdb := cache.GetMemDb()
	states := make(map[string]*KeyState, 0)
	memdb.ForEach(func(key, val []byte) {
		if this.isAccountUpdateStore(key, val) == false {
			return
		}
		accountAddr, _ := common.AddressParseFromBytes(key[common.ADDR_LEN+1:])
		item, ok := states[hex.EncodeToString(accountAddr[:])]
		if !ok {
			states[hex.EncodeToString(accountAddr[:])] = &KeyState{
				Key: accountAddr[:],
				Value: val,
			}
		} else {
			item.Value = append(item.Value, val...)
		}
	})
	stateSlice := make([]*KeyState, 0)
	for _, value := range states {
		stateSlice = append(stateSlice, value)
	}
	sort.Sort(KeyStateSlice(stateSlice))
	hashs := make([]common.Uint256, 0)
	for _, item := range states {
		state := sha256.New()
		var result common.Uint256
		state.Write(item.Value)
		state.Sum(result[:0])
		hashs = append(hashs, result)
	}
	if len(hashs) == 0 {
		return common.UINT256_EMPTY, nil
	} else {
		return merkle.TreeHasher{}.HashFullTreeWithLeafHash(hashs), hashs
	}
}

func calculateTotalStateHash(overlay *overlaydb.OverlayDB) (result common.Uint256, err error) {
	stateDiff := sha256.New()
	iter := overlay.NewIterator([]byte{byte(scom.ST_CONTRACT)})
	err = accumulateHash(stateDiff, iter)
	iter.Release()
	if err != nil {
		return
	}

	iter = overlay.NewIterator([]byte{byte(scom.ST_STORAGE)})
	err = accumulateHash(stateDiff, iter)
	iter.Release()
	if err != nil {
		return
	}

	stateDiff.Sum(result[:0])
	return
}

func accumulateHash(hasher hash.Hash, iter scom.StoreIterator) error {
	for has := iter.First(); has; has = iter.Next() {
		key := iter.Key()
		val := iter.Value()
		hasher.Write(key)
		hasher.Write(val)
	}
	return iter.Error()
}

func (this *LedgerStoreImp) saveBlockToStateStore(block *types.Block, result store.ExecuteResult) error {
	blockHash := block.Hash()
	blockHeight := block.Header.Height

	for _, notify := range result.Notify {
		SaveNotify(this.eventStore, notify.TxHash, notify)
	}

	err := this.stateStore.AddStateMerkleTreeRoot(blockHeight, result.Hash)
	if err != nil {
		return fmt.Errorf("AddBlockMerkleTreeRoot error %s", err)
	}

	err = this.stateStore.AddBlockMerkleTreeRoot(block.Header.TransactionsRoot)
	if err != nil {
		return fmt.Errorf("AddBlockMerkleTreeRoot error %s", err)
	}

	err = this.stateStore.SaveCurrentBlock(blockHeight, blockHash)
	if err != nil {
		return fmt.Errorf("SaveCurrentBlock error %s", err)
	}

	err = this.stateStore.SaveLayer2States(blockHeight, result.UpdatedAccountState)
	if err != nil {
		return fmt.Errorf("SaveLayer2States error %s", err)
	}

	log.Debugf("the state transition hash of block %d is:%s", blockHeight, result.Hash.ToHexString())

	result.WriteSet.ForEach(func(key, val []byte) {
		if len(val) == 0 {
			this.stateStore.BatchDeleteRawKey(key)
		} else {
			this.stateStore.BatchPutRawKeyVal(key, val)
		}
	})

	return nil
}

func (this *LedgerStoreImp) saveBlockToEventStore(block *types.Block) {
	blockHash := block.Hash()
	blockHeight := block.Header.Height
	txs := make([]common.Uint256, 0)
	for _, tx := range block.Transactions {
		txHash := tx.Hash()
		txs = append(txs, txHash)
	}
	if len(txs) > 0 {
		this.eventStore.SaveEventNotifyByBlock(block.Header.Height, txs)
	}
	this.eventStore.SaveCurrentBlock(blockHeight, blockHash)
}

func (this *LedgerStoreImp) tryGetSavingBlockLock() (hasLocked bool) {
	select {
	case this.savingBlockSemaphore <- true:
		return false
	default:
		return true
	}
}

func (this *LedgerStoreImp) getSavingBlockLock() {
	this.savingBlockSemaphore <- true
}

func (this *LedgerStoreImp) releaseSavingBlockLock() {
	select {
	case <-this.savingBlockSemaphore:
		return
	default:
		panic("can not release in unlocked state")
	}
}

//saveBlock do the job of execution samrt contract and commit block to store.
func (this *LedgerStoreImp) submitBlock(block *types.Block, layer2Msg *types.Layer2State, result store.ExecuteResult) error {
	blockHash := block.Hash()
	blockHeight := block.Header.Height
	blockRoot := this.GetBlockRootWithNewTxRoots(block.Header.Height, []common.Uint256{block.Header.TransactionsRoot})
	if block.Header.Height != 0 && blockRoot != block.Header.BlockRoot {
		return fmt.Errorf("wrong block root at height:%d, expected:%s, got:%s",
			block.Header.Height, blockRoot.ToHexString(), block.Header.BlockRoot.ToHexString())
	}

	this.blockStore.NewBatch()
	this.stateStore.NewBatch()
	this.eventStore.NewBatch()
	err := this.saveBlockToBlockStore(block)
	if err != nil {
		return fmt.Errorf("save to block store height:%d error:%s", blockHeight, err)
	}
	err = this.layer2Store.SaveMsgToLayer2Store(layer2Msg)
	if err != nil {
		return fmt.Errorf("save to msg layer2 state store height:%d error:%s", blockHeight, err)
	}
	err = this.saveBlockToStateStore(block, result)
	if err != nil {
		return fmt.Errorf("save to state store height:%d error:%s", blockHeight, err)
	}
	this.saveBlockToEventStore(block)
	err = this.blockStore.CommitTo()
	if err != nil {
		return fmt.Errorf("blockStore.CommitTo height:%d error %s", blockHeight, err)
	}
	// event store is idempotent to re-save when in recovering process, so save first before stateStore
	err = this.eventStore.CommitTo()
	if err != nil {
		return fmt.Errorf("eventStore.CommitTo height:%d error %s", blockHeight, err)
	}
	err = this.stateStore.CommitTo()
	if err != nil {
		return fmt.Errorf("stateStore.CommitTo height:%d error %s", blockHeight, err)
	}
	this.setCurrentBlock(blockHeight, blockHash)

	if events.DefActorPublisher != nil {
		events.DefActorPublisher.Publish(
			message.TOPIC_SAVE_BLOCK_COMPLETE,
			&message.SaveBlockCompleteMsg{
				Block: block,
			})
	}
	return nil
}

func (this *LedgerStoreImp) handleTransaction(overlay *overlaydb.OverlayDB, cache *storage.CacheDB, gasTable map[string]uint64,
	block *types.Block, tx *types.Transaction) (*event.ExecuteNotify, error) {
	txHash := tx.Hash()
	notify := &event.ExecuteNotify{TxHash: txHash, State: event.CONTRACT_STATE_FAIL}
	var err error
	switch tx.TxType {
	case types.Deploy:
		err = this.stateStore.HandleDeployTransaction(this, overlay, gasTable, cache, tx, block, notify)
		if overlay.Error() != nil {
			return nil, fmt.Errorf("HandleDeployTransaction tx %s error %s", txHash.ToHexString(), overlay.Error())
		}
		if err != nil {
			log.Debugf("HandleDeployTransaction tx %s error %s", txHash.ToHexString(), err)
		}
	case types.InvokeNeo:
		err = this.stateStore.HandleInvokeTransaction(this, overlay, gasTable, cache, tx, block, notify)
		if overlay.Error() != nil {
			return nil, fmt.Errorf("HandleInvokeTransaction tx %s error %s", txHash.ToHexString(), overlay.Error())
		}
		if err != nil {
			log.Debugf("HandleInvokeTransaction tx %s error %s", txHash.ToHexString(), err)
		}
	}
	return notify, nil
}

func (this *LedgerStoreImp) saveHeaderIndexList() error {
	this.lock.RLock()
	storeCount := this.storedIndexCount
	currHeight := this.currBlockHeight
	if currHeight-storeCount < HEADER_INDEX_BATCH_SIZE {
		this.lock.RUnlock()
		return nil
	}

	headerList := make([]common.Uint256, HEADER_INDEX_BATCH_SIZE)
	for i := uint32(0); i < HEADER_INDEX_BATCH_SIZE; i++ {
		height := storeCount + i
		headerList[i] = this.headerIndex[height]
	}
	this.lock.RUnlock()

	this.blockStore.SaveHeaderIndexList(storeCount, headerList)

	this.lock.Lock()
	this.storedIndexCount += HEADER_INDEX_BATCH_SIZE
	this.lock.Unlock()
	return nil
}

//IsContainBlock return whether the block is in store
func (this *LedgerStoreImp) IsContainBlock(blockHash common.Uint256) (bool, error) {
	return this.blockStore.ContainBlock(blockHash)
}

//IsContainTransaction return whether the transaction is in store. Wrap function of BlockStore.ContainTransaction
func (this *LedgerStoreImp) IsContainTransaction(txHash common.Uint256) (bool, error) {
	return this.blockStore.ContainTransaction(txHash)
}

//GetBlockRootWithNewTxRoots return the block root(merkle root of blocks) after add a new tx root of block
func (this *LedgerStoreImp) GetBlockRootWithNewTxRoots(startHeight uint32, txRoots []common.Uint256) common.Uint256 {
	this.lock.RLock()
	defer this.lock.RUnlock()
	// the block height in consensus is far behind ledger, this case should be rare
	if this.currBlockHeight > startHeight+uint32(len(txRoots))-1 {
		// or return error?
		return common.UINT256_EMPTY
	} else if this.currBlockHeight+1 < startHeight {
		// this should never happen in normal case
		log.Fatalf("GetBlockRootWithNewTxRoots: invalid param: curr height: %d, start height: %d",
			this.currBlockHeight, startHeight)

		return common.UINT256_EMPTY
	}

	needs := txRoots[this.currBlockHeight+1-startHeight:]
	return this.stateStore.GetBlockRootWithNewTxRoots(needs)
}

func (this *LedgerStoreImp) GetLayer2State(height uint32) (*types.Layer2State, error) {
	return this.layer2Store.GetLayer2State(height)
}

func (this *LedgerStoreImp) GetLayer2StateProof(height uint32, key []byte) ([]byte, error) {
	hashs, err := this.stateStore.GetLayer2States(height)
	if err != nil {
		return nil, fmt.Errorf("GetLayer2StateProof:%s", err)
	}
	path, err := merkle.MerkleLeafPath(key, hashs)
	if err != nil {
		return nil, err
	}
	return path, nil
}

//GetBlockHash return the block hash by block height
func (this *LedgerStoreImp) GetBlockHash(height uint32) common.Uint256 {
	return this.getHeaderIndex(height)
}

//GetHeaderByHash return the block header by block hash
func (this *LedgerStoreImp) GetHeaderByHash(blockHash common.Uint256) (*types.Header, error) {
	return this.blockStore.GetHeader(blockHash)
}

func (this *LedgerStoreImp) GetRawHeaderByHash(blockHash common.Uint256) (*types.RawHeader, error) {
	return this.blockStore.GetRawHeader(blockHash)
}

//GetHeaderByHash return the block header by block height
func (this *LedgerStoreImp) GetHeaderByHeight(height uint32) (*types.Header, error) {
	blockHash := this.GetBlockHash(height)
	return this.GetHeaderByHash(blockHash)
}

//GetSysFeeAmount return the sys fee for block by block hash. Wrap function of BlockStore.GetSysFeeAmount
func (this *LedgerStoreImp) GetSysFeeAmount(blockHash common.Uint256) (common.Fixed64, error) {
	return this.blockStore.GetSysFeeAmount(blockHash)
}

//GetTransaction return transaction by transaction hash. Wrap function of BlockStore.GetTransaction
func (this *LedgerStoreImp) GetTransaction(txHash common.Uint256) (*types.Transaction, uint32, error) {
	return this.blockStore.GetTransaction(txHash)
}

//GetBlockByHash return block by block hash. Wrap function of BlockStore.GetBlockByHash
func (this *LedgerStoreImp) GetBlockByHash(blockHash common.Uint256) (*types.Block, error) {
	return this.blockStore.GetBlock(blockHash)
}

//GetBlockByHeight return block by height.
func (this *LedgerStoreImp) GetBlockByHeight(height uint32) (*types.Block, error) {
	blockHash := this.GetBlockHash(height)
	var empty common.Uint256
	if blockHash == empty {
		return nil, nil
	}
	return this.GetBlockByHash(blockHash)
}

//GetBookkeeperState return the bookkeeper state. Wrap function of StateStore.GetBookkeeperState
func (this *LedgerStoreImp) GetBookkeeperState() (*states.BookkeeperState, error) {
	return this.stateStore.GetBookkeeperState()
}

//GetMerkleProof return the block merkle proof. Wrap function of StateStore.GetMerkleProof
func (this *LedgerStoreImp) GetMerkleProof(proofHeight, rootHeight uint32) ([]common.Uint256, error) {
	return this.stateStore.GetMerkleProof(proofHeight, rootHeight)
}

//GetContractState return contract by contract address. Wrap function of StateStore.GetContractState
func (this *LedgerStoreImp) GetContractState(contractHash common.Address) (*payload.DeployCode, error) {
	return this.stateStore.GetContractState(contractHash)
}

//GetStorageItem return the storage value of the key in smart contract. Wrap function of StateStore.GetStorageState
func (this *LedgerStoreImp) GetStorageItem(key *states.StorageKey) (*states.StorageItem, error) {
	return this.stateStore.GetStorageState(key)
}

//GetEventNotifyByTx return the events notify gen by executing of smart contract.  Wrap function of EventStore.GetEventNotifyByTx
func (this *LedgerStoreImp) GetEventNotifyByTx(tx common.Uint256) (*event.ExecuteNotify, error) {
	return this.eventStore.GetEventNotifyByTx(tx)
}

//GetEventNotifyByBlock return the transaction hash which have event notice after execution of smart contract. Wrap function of EventStore.GetEventNotifyByBlock
func (this *LedgerStoreImp) GetEventNotifyByBlock(height uint32) ([]*event.ExecuteNotify, error) {
	return this.eventStore.GetEventNotifyByBlock(height)
}

//PreExecuteContract return the result of smart contract execution without commit to store
func (this *LedgerStoreImp) PreExecuteContractBatch(txes []*types.Transaction, atomic bool) ([]*sstate.PreExecResult, uint32, error) {
	if atomic {
		this.getSavingBlockLock()
		defer this.releaseSavingBlockLock()
	}
	height := this.GetCurrentBlockHeight()
	results := make([]*sstate.PreExecResult, 0, len(txes))
	for _, tx := range txes {
		res, err := this.PreExecuteContract(tx)
		if err != nil {
			return nil, height, err
		}

		results = append(results, res)
	}

	return results, height, nil
}

//PreExecuteContract return the result of smart contract execution without commit to store
func (this *LedgerStoreImp) PreExecuteContractWithParam(tx *types.Transaction, preParam PrexecuteParam) (*sstate.PreExecResult, error) {
	height := this.GetCurrentBlockHeight()
	// use previous block time to make it predictable for easy test
	blockTime := uint32(time.Now().Unix())
	if header, err := this.GetHeaderByHeight(height); err == nil {
		blockTime = header.Timestamp + 1
	}
	stf := &sstate.PreExecResult{State: event.CONTRACT_STATE_FAIL, Gas: neovm.MIN_TRANSACTION_GAS, Result: nil}

	sconfig := &smartcontract.Config{
		Time:      blockTime,
		Height:    height + 1,
		Tx:        tx,
		BlockHash: this.GetBlockHash(height),
	}

	overlay := this.stateStore.NewOverlayDB()
	cache := storage.NewCacheDB(overlay)
	gasTable := make(map[string]uint64)
	neovm.GAS_TABLE.Range(func(k, value interface{}) bool {
		key := k.(string)
		val := value.(uint64)
		if key == config.WASM_GAS_FACTOR && preParam.WasmFactor != 0 {
			gasTable[key] = preParam.WasmFactor
		} else {
			gasTable[key] = val
		}

		return true
	})

	if tx.TxType == types.InvokeNeo {
		invoke := tx.Payload.(*payload.InvokeCode)

		sc := smartcontract.SmartContract{
			Config:       sconfig,
			Store:        this,
			CacheDB:      cache,
			GasTable:     gasTable,
			Gas:          math.MaxUint64 - calcGasByCodeLen(len(invoke.Code), gasTable[neovm.UINT_INVOKE_CODE_LEN_NAME]),
			WasmExecStep: config.DEFAULT_WASM_MAX_STEPCOUNT,
			JitMode:      preParam.JitMode,
			PreExec:      true,
		}
		//start the smart contract executive function
		engine, _ := sc.NewExecuteEngine(invoke.Code, tx.TxType)

		result, err := engine.Invoke()
		if err != nil {
			return stf, err
		}
		gasCost := math.MaxUint64 - sc.Gas

		if preParam.MinGas {
			mixGas := neovm.MIN_TRANSACTION_GAS
			if gasCost < mixGas {
				gasCost = mixGas
			}
			gasCost = tuneGasFeeByHeight(sconfig.Height, gasCost, neovm.MIN_TRANSACTION_GAS, math.MaxUint64)
		}

		var cv interface{}
		if tx.TxType == types.InvokeNeo { //neovm
			if result != nil {
				val := result.(*types2.VmValue)
				cv, err = val.ConvertNeoVmValueHexString()
				if err != nil {
					return stf, err
				}
			}
		} else { //wasmvm
			cv = common.ToHexString(result.([]byte))
		}

		return &sstate.PreExecResult{State: event.CONTRACT_STATE_SUCCESS, Gas: gasCost, Result: cv, Notify: sc.Notifications}, nil
	} else if tx.TxType == types.Deploy {
		deploy := tx.Payload.(*payload.DeployCode)
		{
			wasmMagicversion := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

			if len(deploy.GetRawCode()) >= len(wasmMagicversion) {
				if bytes.Compare(wasmMagicversion, deploy.GetRawCode()[:8]) == 0 {
					return stf, errors.NewErr("this code is wasm binary. can not deployed as neo contract")
				}
			}
		}

		return &sstate.PreExecResult{State: event.CONTRACT_STATE_SUCCESS, Gas: gasTable[neovm.CONTRACT_CREATE_NAME] + calcGasByCodeLen(len(deploy.GetRawCode()), gasTable[neovm.UINT_DEPLOY_CODE_LEN_NAME]), Result: nil}, nil
	} else {
		return stf, errors.NewErr("transaction type error")
	}
}

//PreExecuteContract return the result of smart contract execution without commit to store
func (this *LedgerStoreImp) PreExecuteContract(tx *types.Transaction) (*sstate.PreExecResult, error) {
	param := PrexecuteParam{
		JitMode:    false,
		WasmFactor: 0,
		MinGas:     true,
	}

	return this.PreExecuteContractWithParam(tx, param)
}

//Close ledger store.
func (this *LedgerStoreImp) Close() error {
	// wait block saving complete, and get the lock to avoid subsequent block saving
	this.getSavingBlockLock()
	defer this.releaseSavingBlockLock()

	this.closing = true

	err := this.blockStore.Close()
	if err != nil {
		return fmt.Errorf("blockStore close error %s", err)
	}
	err = this.eventStore.Close()
	if err != nil {
		return fmt.Errorf("eventStore close error %s", err)
	}
	err = this.stateStore.Close()
	if err != nil {
		return fmt.Errorf("stateStore close error %s", err)
	}
	return nil
}
