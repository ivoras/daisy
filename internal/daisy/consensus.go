package daisy

import (
	"fmt"

	"github.com/ivoras/daisy/internal/dcrypto"
)

/*
 * Choosing between competing branches of the block tree.
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
 *  - With one signatory, all the blocks score 2, and the best chain is just the longest chain, as before.
 *
 * Only the choice of the branch depends on the scores, not the validity of the blocks, so all the blocks which were
 * valid before are still valid.
 */
const (
	scoreInTurn    = 2
	scoreOutOfTurn = 1

	// A branch which forks off the best chain more than this many blocks below its tip is ignored. Otherwise a
	// long forgotten (maybe revoked) key could be used to rewrite history.
	maxReorgDepth = 128
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

// validateBlock checks everything about a block that depends on the chain which it extends, given the signatory
// keys as of the previous block. It doesn't change anything. It returns the key operations of the block.
func validateBlock(blk *Block, height int, keys *keySet) (map[string][]BlockKeyOp, error) {
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
	if err = dcrypto.VerifyHexBytes(sigPubKey, blk.PreviousBlockHash, blk.PreviousBlockHashSignature); err != nil {
		return nil, fmt.Errorf("Verification of previous block hash has failed: %v", err)
	}
	if err = dcrypto.VerifyHexBytes(sigPubKey, blk.Hash, blk.HashSignature); err != nil {
		return nil, fmt.Errorf("Verification of block hash has failed: %v", err)
	}
	ops, err := blk.dbGetKeyOps()
	if err != nil {
		return nil, err
	}
	if err = validateKeyOps(ops, height, keys); err != nil {
		return nil, err
	}
	return ops, nil
}

// validateKeyOps checks the key operations of a block of the given height: each of them must be signed by a quorum
// of different signatories, and it must make sense (only keys which are not known are added, only keys which are
// valid are revoked).
func validateKeyOps(ops map[string][]BlockKeyOp, height int, keys *keySet) error {
	quorum := QuorumForHeight(height)
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
			if err = dcrypto.VerifyPublicKeyHashSignature(sigPubKey, key, keyOp.signature); err != nil {
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
