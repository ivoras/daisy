package daisy

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ivoras/daisy/internal/dcrypto"
)

/*
 * The rules of the block tree.
 *
 * Choosing between competing branches.
 *
 * Any signatory may sign a block at any time, so two of them can extend the same block at the same time, and
 * the blockchain forks. To make all the nodes pick the same branch, every block gets a score, and the branch
 * whose blocks have the highest total score is the best chain (ties are broken by the lowest block hash).
 *
 * The signatories take turns: the signatory whose turn it is at a given height is the one that is at position
 * (height modulo the number of signatories) in the sorted list of the signatories' key hashes. A block signed by the
 * signatory whose turn it is scores 2, a block signed by anybody else scores 1. (This is how the Clique PoA
 * consensus of Ethereum chooses between branches, too.) That means:
 *
 *  - If the signatories keep to the turns, a fork is resolved in favour of the block of the signatory in turn, and it
 *    is not a race of who is faster, or who has the better connectivity.
 *  - Nobody is prevented from signing out of turn (so a signatory that's offline doesn't block the others), but
 *    such blocks lose to the blocks which are in turn, and a branch of out of turn blocks needs to be twice as long.
 *  - With one signatory, all the blocks score 2, and the best chain is just the longest chain.
 *
 * Limiting how fast a branch can grow.
 *
 * The score alone is not enough: a single rogue signatory could sign hundreds of blocks in a few seconds, and
 * replace up to maxReorgDepth blocks of honest history with a longer branch. So there are two more rules, which
 * make a block invalid (and not just a loser):
 *
 *  - Timestamps. Every block above the genesis block has a Timestamp in its _meta table (which is part of the hashed
 *    file). It must be at least the block period (ChainParams.BlockPeriodSeconds, one second if not set) later than
 *    the timestamp of the previous block, and no more than maxFutureDrift in the future by the clock of the node
 *    which is checking the block. So a branch can't grow faster than one block per period, and can't get ahead of the
 *    wall clock by more than the drift. The future check is only made when a block arrives; when the blockchain is
 *    verified again later, only the order of the timestamps is checked (a block that was fine then, is fine now).
 *    The file modification time, which is used for the timestamp of the blocks which don't have one, doesn't count.
 *  - Recently signed (as in Clique). With N active signatories, a signatory may sign at most one of every
 *    floor(N/2)+1 consecutive blocks: the signers of the previous floor(N/2) blocks of the same branch can't sign the
 *    block. So a minority of the signatories can't build a branch alone, not even slowly, because they need the
 *    others to sign the blocks in between. The price is availability: with N=2 both signatories must be online and
 *    take turns (neither can sign two blocks in a row), with N=3 or N=4 nobody can sign two blocks in a row, and so on.
 *    With N=1 there is no constraint. The genesis block is not counted as signed by anybody. Adding or revoking a
 *    key changes N from the next block on.
 *
 * Key operations.
 *
 * A key operation (adding or revoking a signatory key) needs a quorum of signatures of the active signatories (see
 * EffectiveQuorum). The signatures are over a message which says which operation is signed, and on which chain
 * (dcrypto.KeyOpMessage), so they can't be reused for another operation, or on another chain. The genesis block is
 * the exception: the key operations in it are signed in the old format, which only signs the hash of the key (the
 * embedded genesis block is made that way, and it can't change, as its hash is the identity of the chain). The
 * genesis block is pinned by its hash anyway, so there is exactly one genesis rule: the old format, and high-S
 * signatures are fine. All the other signatures (of key ops, and of block hashes) must be low-S.
 *
 * Only the choice of the branch depends on the scores, not the validity of the blocks.
 */
const (
	scoreInTurn    = 2
	scoreOutOfTurn = 1

	// A branch which forks off the best chain more than this many blocks below its tip is ignored. Otherwise a
	// long forgotten (maybe revoked) key could be used to rewrite history.
	maxReorgDepth = 128

	// A block can't have a timestamp which is later than this by the clock of the node which checks it.
	maxFutureDrift = 15 * time.Second
)

// inTurnSigner returns the hash of the key of the signatory whose turn it is to sign the block of the given height
func inTurnSigner(keys *keySet, height int) string {
	active := keys.active()
	if len(active) == 0 {
		return ""
	}
	return active[height%len(active)]
}

// blockScore calculates the score of a block of the given height signed by signer, given the keys as they are
// before the block (i.e. after the previous block)
func blockScore(keys *keySet, height int, signer string) int {
	if height == genesisBlockHeight {
		return 0
	}
	if inTurnSigner(keys, height) == signer {
		return scoreInTurn
	}
	return scoreOutOfTurn
}

// isBetterTip tells if a branch ending in candidate should replace the one ending in current as the best chain
func isBetterTip(candidate, current *DbBlockchainBlock) bool {
	if candidate.TotalScore != current.TotalScore {
		return candidate.TotalScore > current.TotalScore
	}
	return candidate.Hash < current.Hash
}

// recentSignerWindow returns how many of the last blocks of a branch can't be signed again by their signers, given the
// keys as of the previous block: a signatory may sign one of every window+1 consecutive blocks.
func recentSignerWindow(keys *keySet) int {
	return len(keys.active()) / 2
}

// EffectiveQuorum returns the number of signatures which a key operation in a block of the given height needs, if
// there are the given number of active signatories before the block. It's QuorumForHeight(), but never more than a
// majority of the signatories, so that a chain with few signatories can still add and revoke keys.
func EffectiveQuorum(height, activeSigners int) int {
	return min(QuorumForHeight(height), activeSigners/2+1)
}

// blockContext is what validateBlock needs to know about the chain and the world, besides the signatory keys
type blockContext struct {
	// The hash of the genesis block, which the key operations are bound to
	genesisHash string
	// The timestamp of the previous block. Zero if the previous block is a genesis block which doesn't have one.
	parentTime time.Time
	// The current time. If zero, the timestamp of the block is not checked against it.
	now time.Time
	// The minimum difference between the timestamps of a block and the one before it
	blockPeriod time.Duration
	// The signers of the blocks before this one on its branch, the last one being the signer of the previous block.
	// Only the last recentSignerWindow(keys) entries are used. The genesis block is not included.
	recentSigners []string
}

// validateBlock checks everything about a block that depends on the chain which it extends, given the signatory
// keys as of the previous block and the context. It doesn't change anything. It returns the key operations of the block.
func validateBlock(blk *Block, height int, keys *keySet, ctx blockContext) (map[string][]BlockKeyOp, error) {
	if blk.Version != CurrentBlockVersion {
		return nil, fmt.Errorf("Unsupported block version: %d", blk.Version)
	}
	signatory, ok := keys.get(blk.SignaturePublicKeyHash)
	if !ok {
		if keys.known(blk.SignaturePublicKeyHash) {
			return nil, fmt.Errorf("The public key %s signing the block is revoked", blk.SignaturePublicKeyHash)
		}
		return nil, fmt.Errorf("Cannot find an accepted public key %s signing the block", blk.SignaturePublicKeyHash)
	}
	sigPubKey, err := dcrypto.DecodePublicKeyBytes(signatory.publicKey)
	if err != nil {
		return nil, fmt.Errorf("Cannot decode public key %s: %v", blk.SignaturePublicKeyHash, err)
	}
	if err = dcrypto.VerifyHexBytesLowS(sigPubKey, blk.PreviousBlockHash, blk.PreviousBlockHashSignature); err != nil {
		return nil, fmt.Errorf("Verification of previous block hash has failed: %v", err)
	}
	if err = dcrypto.VerifyHexBytesLowS(sigPubKey, blk.Hash, blk.HashSignature); err != nil {
		return nil, fmt.Errorf("Verification of block hash has failed: %v", err)
	}
	if err = validateTimestamp(blk, ctx); err != nil {
		return nil, err
	}
	if window := recentSignerWindow(keys); window > 0 {
		recent := ctx.recentSigners
		if len(recent) > window {
			recent = recent[len(recent)-window:]
		}
		if slices.Contains(recent, blk.SignaturePublicKeyHash) {
			return nil, fmt.Errorf("The public key %s signed one of the previous %d blocks, and there are %d signatories, so it can't sign this block",
				blk.SignaturePublicKeyHash, window, len(keys.active()))
		}
	}
	ops, err := blk.dbGetKeyOps()
	if err != nil {
		return nil, err
	}
	if err = validateKeyOps(ops, height, keys, ctx.genesisHash); err != nil {
		return nil, err
	}
	return ops, nil
}

// validateTimestamp checks the timestamp of a block against the one of the previous block, and the current time
func validateTimestamp(blk *Block, ctx blockContext) error {
	if blk.Timestamp.IsZero() {
		return errors.New("The block has no valid Timestamp in its metadata")
	}
	if !ctx.parentTime.IsZero() && blk.Timestamp.Before(ctx.parentTime.Add(ctx.blockPeriod)) {
		return fmt.Errorf("The block timestamp %s is not at least %s later than the timestamp of the previous block, %s",
			blk.Timestamp.Format(time.RFC3339), ctx.blockPeriod, ctx.parentTime.Format(time.RFC3339))
	}
	if !ctx.now.IsZero() && blk.Timestamp.After(ctx.now.Add(maxFutureDrift)) {
		return fmt.Errorf("The block timestamp %s is more than %s in the future", blk.Timestamp.Format(time.RFC3339), maxFutureDrift)
	}
	return nil
}

// validateKeyOps checks the key operations of a block of the given height: each of them must be signed by a quorum
// of different signatories, and it must make sense (only keys which are not known are added, only keys which are
// valid are revoked).
func validateKeyOps(ops map[string][]BlockKeyOp, height int, keys *keySet, genesisHash string) error {
	quorum := EffectiveQuorum(height, len(keys.active()))
	for key, keyOps := range ops {
		signers := make(map[string]bool)
		for _, keyOp := range keyOps {
			signer, ok := keys.get(keyOp.signatureKeyHash)
			if !ok {
				return fmt.Errorf("Key op on %s is signed by %s which is not a valid signatory", key, keyOp.signatureKeyHash)
			}
			if signers[keyOp.signatureKeyHash] {
				return fmt.Errorf("Key op on %s is signed more than once by %s", key, keyOp.signatureKeyHash)
			}
			signers[keyOp.signatureKeyHash] = true
			sigPubKey, err := dcrypto.DecodePublicKeyBytes(signer.publicKey)
			if err != nil {
				return fmt.Errorf("Cannot decode public key %s: %v", keyOp.signatureKeyHash, err)
			}
			if err = dcrypto.VerifyKeyOp(sigPubKey, genesisHash, keyOp.op, key, keyOp.signature); err != nil {
				return fmt.Errorf("Failed verification of key op for %s by %s", key, keyOp.signatureKeyHash)
			}
		}
		if len(signers) < quorum {
			return fmt.Errorf("Quorum of %d not met for key ops on key %s", quorum, key)
		}
		switch keyOps[0].op {
		case "A":
			if keys.known(key) {
				return fmt.Errorf("Attempt to add an already existing key to the list of signatories: %s", key)
			}
		case "R":
			if _, ok := keys.get(key); !ok {
				return fmt.Errorf("Cannot revoke a key which is not valid: %s", key)
			}
		default:
			return fmt.Errorf("Invalid key op: %s", keyOps[0].op)
		}
	}
	return nil
}
