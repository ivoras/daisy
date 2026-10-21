package daisy

import "slices"

// noHeight is the height of a block which doesn't exist: a key which isn't revoked has been revoked at noHeight.
const noHeight = -1

// keyInfo describes a signatory key as the blockchain knows it
type keyInfo struct {
	hash         string
	publicKey    []byte
	addHeight    int // The height of the block which added the key
	revokeHeight int // The height of the block which revoked the key, or noHeight
}

// keySet is the set of signatory keys as of some block of the blockchain: the keys which have been added
// by the blocks up to that one, and the ones which have been revoked by them. It's the state which is needed to
// validate the block which comes after that one. Every branch of the block tree has its own, and it can be
// moved back and forth in the chain (see rewind), which is what makes it possible to validate side branches.
type keySet struct {
	keys map[string]*keyInfo
}

func newKeySet() *keySet {
	return &keySet{keys: make(map[string]*keyInfo)}
}

func (ks *keySet) clone() *keySet {
	c := newKeySet()
	for h, ki := range ks.keys {
		kc := *ki
		c.keys[h] = &kc
	}
	return c
}

// known tells if the key has ever been added, whether it has been revoked or not
func (ks *keySet) known(hash string) bool {
	_, ok := ks.keys[hash]
	return ok
}

// get returns a key which is currently allowed to sign blocks
func (ks *keySet) get(hash string) (*keyInfo, bool) {
	ki, ok := ks.keys[hash]
	if !ok || ki.revokeHeight != noHeight {
		return nil, false
	}
	return ki, true
}

// active returns the hashes of the keys which may sign blocks, in the order which defines the signing turns.
func (ks *keySet) active() []string {
	var result []string
	for h, ki := range ks.keys {
		if ki.revokeHeight == noHeight {
			result = append(result, h)
		}
	}
	slices.Sort(result)
	return result
}

// rewind makes the key set the way it was after the block of the given height, by undoing the key operations of
// all the later blocks.
func (ks *keySet) rewind(height int) {
	for h, ki := range ks.keys {
		if ki.addHeight > height {
			delete(ks.keys, h)
		} else if ki.revokeHeight > height {
			ki.revokeHeight = noHeight
		}
	}
}

// apply adds the key operations of the block of the given height. The operations must have been validated.
func (ks *keySet) apply(height int, ops map[string][]BlockKeyOp) {
	for hash, keyOps := range ops {
		switch keyOps[0].op {
		case "A":
			ks.keys[hash] = &keyInfo{hash: hash, publicKey: keyOps[0].publicKeyBytes, addHeight: height, revokeHeight: noHeight}
		case "R":
			if ki, ok := ks.keys[hash]; ok {
				ki.revokeHeight = height
			}
		}
	}
}
