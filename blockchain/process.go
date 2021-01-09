// Copyright (c) 2013-2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"fmt"
	"github.com/classzz/classzz/chaincfg/chainhash"
	"github.com/classzz/classzz/cross"
	"github.com/classzz/classzz/database"
	"github.com/classzz/classzz/txscript"
	"github.com/classzz/czzutil"
)

// BehaviorFlags is a bitmask defining tweaks to the normal behavior when
// performing chain processing and consensus rules checks.
type BehaviorFlags uint32

const (
	// BFFastAdd may be set to indicate that several checks can be avoided
	// for the block since it is already known to fit into the chain due to
	// already proving it correct links into the chain up to a known
	// checkpoint.  This is primarily used for headers-first mode.
	BFFastAdd BehaviorFlags = 1 << iota

	// BFNoPoWCheck may be set to indicate the proof of work check which
	// ensures a block hashes to a value less than the required target will
	// not be performed.
	BFNoPoWCheck

	// BFMagneticAnomaly signals that the magnetic anomaly hardfork is
	// active and the block should be validated according the new rule
	// set.
	BFMagneticAnomaly

	// BFNoDupBlockCheck signals if the block should skip existence
	// checks.
	BFNoDupBlockCheck

	// BFNone is a convenience value to specifically indicate no flags.
	BFNone BehaviorFlags = 0
)

// HasFlag returns whether the BehaviorFlags has the passed flag set.
func (behaviorFlags BehaviorFlags) HasFlag(flag BehaviorFlags) bool {
	return behaviorFlags&flag == flag
}

// blockExists determines whether a block with the given hash exists either in
// the main chain or any side chains.
//
// This function is safe for concurrent access.
func (b *BlockChain) blockExists(hash *chainhash.Hash) (bool, error) {
	// Check block index first (could be main chain or side chain blocks).
	if b.index.HaveBlock(hash) {
		return true, nil
	}

	// Check in the database.
	var exists bool
	err := b.db.View(func(dbTx database.Tx) error {
		var err error
		exists, err = dbTx.HasBlock(hash)
		if err != nil || !exists {
			return err
		}

		// Ignore side chain blocks in the database.  This is necessary
		// because there is not currently any record of the associated
		// block index data such as its block height, so it's not yet
		// possible to efficiently load the block and do anything useful
		// with it.
		//
		// Ultimately the entire block index should be serialized
		// instead of only the current main chain so it can be consulted
		// directly.
		_, err = dbFetchHeightByHash(dbTx, hash)
		if isNotInMainChainErr(err) {
			exists = false
			return nil
		}
		return err
	})
	return exists, err
}

// processOrphans determines if there are any orphans which depend on the passed
// block hash (they are no longer orphans if true) and potentially accepts them.
// It repeats the process for the newly accepted blocks (to detect further
// orphans which may no longer be orphans) until there are no more.
//
// The flags do not modify the behavior of this function directly, however they
// are needed to pass along to maybeAcceptBlock.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) processOrphans(hash *chainhash.Hash, flags BehaviorFlags) error {
	// Start with processing at least the passed hash.  Leave a little room
	// for additional orphan blocks that need to be processed without
	// needing to grow the array in the common case.
	processHashes := make([]*chainhash.Hash, 0, 10)
	processHashes = append(processHashes, hash)
	for len(processHashes) > 0 {
		// Pop the first hash to process from the slice.
		processHash := processHashes[0]
		processHashes[0] = nil // Prevent GC leak.
		processHashes = processHashes[1:]

		// Look up all orphans that are parented by the block we just
		// accepted.  This will typically only be one, but it could
		// be multiple if multiple blocks are mined and broadcast
		// around the same time.  The one with the most proof of work
		// will eventually win out.  An indexing for loop is
		// intentionally used over a range here as range does not
		// reevaluate the slice on each iteration nor does it adjust the
		// index for the modified slice.
		for i := 0; i < len(b.prevOrphans[*processHash]); i++ {
			orphan := b.prevOrphans[*processHash][i]
			if orphan == nil {
				log.Warnf("Found a nil entry at index %d in the "+
					"orphan dependency list for block %v", i,
					processHash)
				continue
			}

			// Remove the orphan from the orphan pool.
			orphanHash := orphan.block.Hash()
			b.removeOrphanBlock(orphan)
			i--

			// Potentially accept the block into the block chain.
			_, err := b.maybeAcceptBlock(orphan.block, flags)
			if err != nil {
				return err
			}

			// Add this block to the list of blocks to process so
			// any orphan blocks that depend on this block are
			// handled too.
			processHashes = append(processHashes, orphanHash)
		}
	}
	return nil
}

// ProcessBlock is the main workhorse for handling insertion of new blocks into
// the block chain.  It includes functionality such as rejecting duplicate
// blocks, ensuring blocks follow all rules, orphan handling, and insertion into
// the block chain along with best chain selection and reorganization.
//
// When no errors occurred during processing, the first return value indicates
// whether or not the block is on the main chain and the second indicates
// whether or not the block is an orphan.
//
// This function is safe for concurrent access.
func (b *BlockChain) ProcessBlock(block *czzutil.Block, flags BehaviorFlags) (bool, bool, error) {
	b.chainLock.Lock()
	defer b.chainLock.Unlock()

	blockHash := block.Hash()
	log.Tracef("Processing block %v", blockHash)

	if !flags.HasFlag(BFNoDupBlockCheck) {
		// The block must not already exist in the main chain or side chains.
		exists, err := b.blockExists(blockHash)
		if err != nil {
			return false, false, err
		}
		if exists {
			str := fmt.Sprintf("already have block %v", blockHash)
			return false, false, ruleError(ErrDuplicateBlock, str)
		}

		// The block must not already exist as an orphan.
		if _, exists := b.orphans[*blockHash]; exists {
			str := fmt.Sprintf("already have block (orphan) %v", blockHash)
			return false, false, ruleError(ErrDuplicateBlock, str)
		}
	}

	flags |= BFMagneticAnomaly

	var err error
	// Handle orphan blocks.
	blockHeader := &block.MsgBlock().Header
	prevHash := &blockHeader.PrevBlock
	prevHashExists, err := b.blockExists(prevHash)
	prevHeader, _ := b.HeaderByHash(prevHash)
	prevHeight, _ := b.BlockHeightByHashAll(prevHash)
	blockHeight := prevHeight + 1

	if err != nil {
		return false, false, err
	}

	var eState *cross.EntangleState
	if b.chainParams.BeaconHeight <= prevHeight && b.chainParams.ConverHeight > prevHeight {
		cState := b.GetCstateByHashAndHeight(*prevHash, prevHeight)
		bai2s := make(map[string]*cross.BeaconAddressInfo)
		for _, v := range cState.PledgeInfos {
			bai2 := &cross.BeaconAddressInfo{
				ExchangeID:      v.ID.Uint64(),
				StakingAmount:   v.StakingAmount,
				CoinBaseAddress: v.CoinBaseAddress,
			}
			bai2s[bai2.Address] = bai2
		}

		eState = &cross.EntangleState{
			EnInfos: bai2s,
		}

	} else if b.chainParams.ConverHeight <= prevHeight {
		eState = b.GetEstateByHashAndHeight(*prevHash, prevHeight)
	}

	script := block.MsgBlock().Transactions[0].TxOut[0].PkScript
	_, addrs, _, _ := txscript.ExtractPkScriptAddrs(script, b.chainParams)

	// Perform preliminary sanity checks on the block and its transactions.
	err = checkBlockSanity(b.chainParams, &prevHeader, block, b.chainParams.PowLimit, b.timeSource, flags, eState, addrs[0])
	if err != nil {
		return false, false, err
	}

	if !prevHashExists {
		log.Infof("Adding orphan block %v with parent %v", blockHash, prevHash)
		b.addOrphanBlock(block)

		return false, true, nil
	}

	if b.chainParams.BeaconHeight < blockHeight && b.chainParams.ConverHeight > blockHeight {
		if err := b.CheckBeacon(block, prevHeight); err != nil {
			return false, false, err
		}
	}

	// cross Verify
	if b.chainParams.ConverHeight <= blockHeight {
		if err := b.CheckBlockCrossTx(block, prevHeight); err != nil {
			return false, false, err
		}
	}

	// The block has passed all context independent checks and appears sane
	// enough to potentially accept it into the block chain.
	isMainChain, err := b.maybeAcceptBlock(block, flags)
	if err != nil {
		return false, false, err
	}

	// Accept any orphan blocks that depend on this block (they are
	// no longer orphans) and repeat for those accepted blocks until
	// there are no more.
	err = b.processOrphans(blockHash, flags)
	if err != nil {
		return false, false, err
	}

	log.Debugf("Accepted block %v", blockHash)

	return isMainChain, false, nil
}
