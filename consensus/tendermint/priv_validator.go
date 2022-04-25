package tendermint

import (
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	pbft "github.com/ethereum/go-ethereum/consensus/tendermint/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var ErrNotAuthorized = errors.New("not authorized to sign this account")

type EthPrivValidator struct {
	signer   common.Address // Ethereum address of the signing key
	signFn   SignerFn       // Signer function to authorize hashes with
	signTxFn SignTxFn       // Function to sign TX
}

type EthPubKey struct {
	signer common.Address
}

func (pubkey *EthPubKey) Type() string {
	return "ETH_PUBKEY"
}

func (pubkey *EthPubKey) Address() common.Address {
	return pubkey.signer
}

func (pubkey *EthPubKey) VerifySignature(msg []byte, sig []byte) bool {
	pub, err := crypto.Ecrecover(msg, sig)
	if err != nil {
		return false
	}

	if len(pub) == 0 || pub[0] != 4 {
		return false
	}

	var signer common.Address
	copy(signer[:], crypto.Keccak256(pub[1:])[12:])
	return signer == pubkey.signer
}

func NewEthPrivValidator(signer common.Address, signFn SignerFn, signTxFn SignTxFn) pbft.PrivValidator {
	return &EthPrivValidator{signer: signer, signFn: signFn, signTxFn: signTxFn}
}

func (pv *EthPrivValidator) Address() common.Address {
	return pv.signer
}

func (pv *EthPrivValidator) GetPubKey(context.Context) (pbft.PubKey, error) {
	return &EthPubKey{signer: pv.signer}, nil
}

func (pv *EthPrivValidator) SignVote(ctx context.Context, chainId string, vote *pbft.Vote) error {
	vote.TimestampMs = uint64(pbft.CanonicalNowMs())
	b := vote.VoteSignBytes(chainId)

	sign, err := pv.signFn(accounts.Account{Address: pv.signer}, accounts.MimetypeClique, b)
	vote.Signature = sign

	return err
}

func (pv *EthPrivValidator) SignProposal(ctx context.Context, chainID string, proposal *pbft.Proposal) error {
	// TODO: sanity check
	b := proposal.ProposalSignBytes(chainID)

	sign, err := pv.signFn(accounts.Account{Address: pv.signer}, accounts.MimetypeClique, b)
	proposal.Signature = sign
	return err
}

func (pv *EthPrivValidator) SignTX(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	return pv.signTxFn(accounts.Account{Address: pv.signer}, tx, chainID)
}
