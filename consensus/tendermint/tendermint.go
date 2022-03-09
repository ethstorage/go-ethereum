// Copyright 2017 The go-ethereum Authors
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

// Package tendermint implements the proof-of-stake consensus engine.
package tendermint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"sync"
	"time"

	pbftconsensus "github.com/QuarkChain/go-minimal-pbft/consensus"
	libp2p "github.com/QuarkChain/go-minimal-pbft/p2p"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/consensus/tendermint/adapter"
	"github.com/ethereum/go-ethereum/consensus/tendermint/gov"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	p2pcrypto "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
)

// Clique proof-of-authority protocol constants.
var (
	epochLength = uint64(30000) // Default number of blocks after which to checkpoint and reset the pending votes

	nonceDefault = hexutil.MustDecode("0x0000000000000000") // Magic nonce number to vote on removing a signer.

	uncleHash = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.

)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errInvalidCheckpointBeneficiary is returned if a checkpoint/epoch transition
	// block has a beneficiary set to non-zeroes.
	errInvalidCheckpointBeneficiary = errors.New("beneficiary in checkpoint block non-zero")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// errInvalidDifficulty is returned if the difficulty of a block neither 1 or 2.
	errInvalidDifficulty = errors.New("invalid difficulty")
)

// Clique is the proof-of-authority consensus engine proposed to support the
// Ethereum testnet following the Ropsten attacks.
type Tendermint struct {
	config        *params.TendermintConfig // Consensus engine configuration parameters
	rootCtxCancel context.CancelFunc
	rootCtx       context.Context

	lock    sync.RWMutex // Protects the signer fields
	privVal pbftconsensus.PrivValidator

	p2pserver *libp2p.Server
}

// New creates a Clique proof-of-authority consensus engine with the initial
// signers set to the ones provided by the user.
func New(config *params.TendermintConfig) *Tendermint {
	// Set any missing consensus parameters to their defaults
	conf := *config
	if conf.Epoch == 0 {
		conf.Epoch = epochLength
	}

	return &Tendermint{
		config: &conf,
	}
}

// SignerFn hashes and signs the data to be signed by a backing account.
type SignerFn func(signer accounts.Account, mimeType string, message []byte) ([]byte, error)

// Authorize injects a private key into the consensus engine to mint new blocks
// with.
func (c *Tendermint) Authorize(signer common.Address, signFn SignerFn) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.privVal = NewEthPrivValidator(signer, signFn)
}

func (c *Tendermint) getPrivValidator() pbftconsensus.PrivValidator {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.privVal
}

func (c *Tendermint) P2pServer() *libp2p.Server {
	return c.p2pserver
}

func (c *Tendermint) Init(chain *core.BlockChain, makeBlock func(parent common.Hash, coinbase common.Address, timestamp uint64) (*types.Block, error)) (err error) {
	// Outbound gossip message queue
	sendC := make(chan pbftconsensus.Message, 1000)

	// Inbound observations
	obsvC := make(chan pbftconsensus.MsgInfo, 1000)

	// Node's main lifecycle context.
	rootCtx, rootCtxCancel := context.WithCancel(context.Background())
	c.rootCtxCancel = rootCtxCancel
	c.rootCtx = rootCtx

	// datastore
	store := adapter.NewStore(chain, c.VerifyHeader, makeBlock)

	// p2p key
	p2pPriv, err := getOrCreateNodeKey(c.config.NodeKeyPath)
	if err != nil {
		return
	}

	// p2p server
	p2pserver, err := libp2p.NewP2PServer(rootCtx, store, obsvC, sendC, p2pPriv, c.config.P2pPort, c.config.NetworkID, c.config.P2pBootstrap, c.config.NodeName, rootCtxCancel)
	if err != nil {
		return
	}

	c.p2pserver = p2pserver

	go func() {
		err := p2pserver.Run(rootCtx)
		if err != nil {
			log.Warn("p2pserver.Run", "err", err)
		}
	}()

	gov := gov.New(c.config.Epoch, chain)
	block := chain.CurrentHeader()
	number := block.Number.Uint64()
	var lastValidators []common.Address
	var lastValidatorPowers []uint64
	if number != 0 {
		lastValidators = gov.EpochValidators(number - 1)
		lastValidatorPowers = gov.EpochValidatorPowers(number - 1)
	}
	gcs := pbftconsensus.MakeChainState(
		c.config.NetworkID,
		number,
		block.Hash(),
		block.TimeMs,
		gov.EpochValidators(number),
		types.U64ToI64Array(gov.EpochValidatorPowers(number)),
		gov.NextValidators(number),
		types.U64ToI64Array(gov.NextValidatorPowers(number)),
		lastValidators,
		types.U64ToI64Array(lastValidatorPowers),
		c.config.Epoch,
		int64(c.config.ProposerRepetition),
	)

	// consensus
	consensusState := pbftconsensus.NewConsensusState(
		rootCtx,
		&c.config.ConsensusConfig,
		*gcs,
		store,
		store,
		obsvC,
		sendC,
	)

	privVal := c.getPrivValidator()
	if privVal != nil {
		consensusState.SetPrivValidator(privVal)
		pubkey, err := privVal.GetPubKey(rootCtx)
		if err != nil {
			panic("fail to get validator address")
		}
		log.Info("Chamber consensus in validator mode", "validator_addr", pubkey.Address())
	}

	err = consensusState.Start(rootCtx)
	if err != nil {
		log.Warn("consensusState.Start", "err", err)
	}

	p2pserver.SetConsensusState(consensusState)

	log.Info("Chamber consensus engine started", "networkd_id", c.config.NetworkID)

	return
}

var TestMode bool

func EnableTestMode() {
	TestMode = true
	libp2p.TestMode = true
}

func getOrCreateNodeKey(path string) (p2pcrypto.PrivKey, error) {
	if path == "" {
		if TestMode {
			priv, _, err := p2pcrypto.GenerateKeyPair(p2pcrypto.Ed25519, -1)
			if err != nil {
				panic(err)
			}
			// don't save priv in test mode
			return priv, nil
		}
		return nil, fmt.Errorf("node key path is empty")
	}
	b, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("No node key found, generating a new one...", "path", path)

			priv, _, err := p2pcrypto.GenerateKeyPair(p2pcrypto.Ed25519, -1)
			if err != nil {
				panic(err)
			}

			s, err := p2pcrypto.MarshalPrivateKey(priv)
			if err != nil {
				panic(err)
			}

			err = ioutil.WriteFile(path, s, 0600)
			if err != nil {
				return nil, fmt.Errorf("failed to write node key: %w", err)
			}

			return priv, nil
		} else {
			return nil, fmt.Errorf("failed to read node key: %w", err)
		}
	}

	priv, err := p2pcrypto.UnmarshalPrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal node key: %w", err)
	}

	peerID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		panic(err)
	}

	log.Info("Found existing node key",
		"path", path,
		"peerID", peerID)

	return priv, nil
}

// Author implements consensus.Engine, returning the Ethereum address recovered
// from the signature in the header's extra-data section.
func (c *Tendermint) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (c *Tendermint) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	return c.verifyHeader(chain, header, nil, seal)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
func (c *Tendermint) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := c.verifyHeader(chain, header, headers[:i], seals[i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (c *Tendermint) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header, seal bool) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	// Don't waste time checking blocks from the future
	if header.Time > uint64(time.Now().Unix()) {
		return consensus.ErrFutureBlock
	}

	governance := gov.New(c.config.Epoch, chain)
	if !gov.CompareValidators(header.NextValidators, governance.NextValidators(number)) {
		return errors.New("NextValidators is incorrect")
	}
	if !gov.CompareValidatorPowers(header.NextValidatorPowers, governance.NextValidatorPowers(number)) {
		return errors.New("NextValidatorPowers is incorrect")
	}
	if len(header.NextValidatorPowers) != len(header.NextValidators) {
		return errors.New("NextValidators must have the same len as powers")
	}
	if !bytes.Equal(header.Nonce[:], nonceDefault) {
		return errors.New("invalid nonce")
	}
	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	// Ensure that the block doesn't contain any uncles which are meaningless in PoA
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}
	// Ensure that the block's difficulty is meaningful (may not be correct at this point)
	if number > 0 {
		if header.Difficulty == nil || (header.Difficulty.Cmp(big.NewInt(1)) != 0) {
			return errInvalidDifficulty
		}
	}
	// Verify that the gas limit is <= 2^63-1
	if header.GasLimit > params.MaxGasLimit {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, params.MaxGasLimit)
	}
	// If all checks passed, validate any special fields for hard forks
	if err := misc.VerifyForkHashes(chain.Config(), header, false); err != nil {
		return err
	}
	// All basic checks passed, verify signatures fields
	if !seal {
		return nil
	}

	epochHeader := c.getEpochHeader(chain, header)
	if epochHeader == nil {
		return fmt.Errorf("epochHeader not found, height:%d", number)
	}

	vs := types.NewValidatorSet(epochHeader.NextValidators, types.U64ToI64Array(epochHeader.NextValidatorPowers), int64(c.config.ProposerRepetition))
	return vs.VerifyCommit(c.config.NetworkID, header.Hash(), number, header.Commit)
}

func (c *Tendermint) getEpochHeader(chain consensus.ChainHeaderReader, header *types.Header) *types.Header {
	number := header.Number.Uint64()
	checkpoint := (number % c.config.Epoch) == 0
	var epochHeight uint64
	if checkpoint {
		epochHeight -= c.config.Epoch
	} else {
		epochHeight = number - (number % c.config.Epoch)
	}
	return chain.GetHeaderByNumber(epochHeight)

}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (c *Tendermint) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (c *Tendermint) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	number := header.Number.Uint64()
	epochHeader := c.getEpochHeader(chain, header)
	if epochHeader == nil {
		return fmt.Errorf("epochHeader not found, height:%d", number)
	}
	parentHeader := chain.GetHeaderByHash(header.ParentHash)
	if epochHeader == nil {
		return fmt.Errorf("parentHeader not found, height:%d", number)
	}

	header.LastCommitHash = parentHeader.Commit.Hash()
	var timestamp uint64
	if number == 1 {
		timestamp = parentHeader.TimeMs // genesis time
	} else {
		timestamp = pbftconsensus.MedianTime(
			parentHeader.Commit,
			types.NewValidatorSet(epochHeader.NextValidators, types.U64ToI64Array(epochHeader.NextValidatorPowers), int64(c.config.ProposerRepetition)),
		)
	}

	header.TimeMs = timestamp
	header.Time = timestamp / 1000
	header.Difficulty = big.NewInt(1)

	governance := gov.New(c.config.Epoch, chain)
	header.NextValidators = governance.NextValidators(number)
	header.NextValidatorPowers = governance.NextValidatorPowers(number)

	return nil
}

// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards given.
func (c *Tendermint) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	// No block rewards at the moment, so the state remains as is and uncles are dropped
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)
}

// FinalizeAndAssemble implements consensus.Engine, ensuring no uncles are set,
// nor block rewards given, and returns the final block.
func (c *Tendermint) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	// Finalize block
	c.Finalize(chain, header, state, txs, uncles)

	// Assemble and return the final block for sealing
	return types.NewBlock(header, txs, nil, receipts, trie.NewStackTrie(nil)), nil
}

// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
func (c *Tendermint) Seal(chain consensus.ChainHeaderReader, block *types.Block, resultCh chan<- *types.Block, stop <-chan struct{}) error {
	panic("should never be called")
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have:
// * DIFF_NOTURN(2) if BLOCK_NUMBER % SIGNER_COUNT != SIGNER_INDEX
// * DIFF_INTURN(1) if BLOCK_NUMBER % SIGNER_COUNT == SIGNER_INDEX
func (c *Tendermint) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	// TOOD: no diff is required
	return big.NewInt(1)
}

// SealHash returns the hash of a block prior to it being sealed.
func (c *Tendermint) SealHash(header *types.Header) common.Hash {
	return header.Hash()
}

// Close implements consensus.Engine. It's a noop for clique as there are no background threads.
func (c *Tendermint) Close() error {
	if c.rootCtxCancel != nil {
		c.rootCtxCancel()
	}

	return nil
}

// APIs implements consensus.Engine, returning the user facing RPC API to allow
// controlling the signer voting.
func (c *Tendermint) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{}
}
