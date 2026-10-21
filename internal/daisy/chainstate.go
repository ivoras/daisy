package daisy

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

/*
 * The block tree.
 *
 * The blocks of the best chain are in the blockchain table and their files are named by height (see
 * blockchainGetFilename). All the other valid blocks, i.e. the other branches, are in the sideblocks table and their
 * files are named by hash, in the "side" directory. When another branch becomes the best chain, the two
 * branches are swapped (reorganize).
 *
 * The signatory keys are kept in the pubkeys table, as of the tip of the best chain. The keys as of any other block
 * are calculated from them by keySet.rewind() and keySet.apply().
 */

// Serializes all changes to the block tree: blocks arrive from many connections at the same time
var chainLock sync.Mutex

type acceptResult int

const (
	blockAlreadyKnown acceptResult = iota
	blockNewTip                    // The block extends the best chain
	blockReorganized               // The block is on another branch, which has become the best chain
	blockOnSideBranch              // The block is valid, but the best chain is better than its branch
	blockOrphan                    // The block that comes before it is not known (yet)
)

func (r acceptResult) String() string {
	return [...]string{"already known", "new tip", "reorganized", "side branch", "orphan"}[r]
}

type acceptInfo struct {
	result  acceptResult
	hash    string
	height  int
	missing string // For orphans: the hash of the block which is needed
}

func (ai acceptInfo) accepted() bool {
	return ai.result == blockNewTip || ai.result == blockReorganized || ai.result == blockOnSideBranch
}

func sideBlockFilename(hash string) string {
	return filepath.Join(blockchainSubdirectory, "side", hash+".db")
}

func orphanDirname() string {
	return filepath.Join(blockchainSubdirectory, "orphans")
}

// The name under which a block file of the new best chain is kept until the database says it's the right one
func stagedBlockFilename(height int) string {
	return blockchainGetFilename(height) + ".new"
}

// Copies a file, creating the destination directory
func copyFileTo(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Reads the key operations from a block file
func readKeyOps(fileName string) (map[string][]BlockKeyOp, error) {
	db, err := dbOpen(fileName, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err = dbCheckBlockTables(db); err != nil {
		return nil, err
	}
	b := Block{db: db}
	return b.dbGetKeyOps()
}

// Reads the Timestamp of a block file. It is zero if there's no valid one.
func readBlockTimestamp(fileName string) (time.Time, error) {
	db, err := dbOpen(fileName, true)
	if err != nil {
		return time.Time{}, err
	}
	defer db.Close()
	if err = dbCheckBlockTables(db); err != nil {
		return time.Time{}, err
	}
	b := Block{db: db}
	t, err := b.dbGetMetaTime("Timestamp")
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// blockFileOf returns the name of the file which has the given block of the block tree
func blockFileOf(block *DbBlockchainBlock, onBestChain bool) string {
	if onBestChain {
		return blockchainGetFilename(block.Height)
	}
	return sideBlockFilename(block.Hash)
}

// recentSignersOf returns the signers of the last n blocks (not counting the genesis block) of the branch which ends
// with the given block, the signer of that block being the last one.
func recentSignersOf(block *DbBlockchainBlock, n int) ([]string, error) {
	var result []string
	cur := block
	for len(result) < n && cur.Height > genesisBlockHeight {
		result = append(result, cur.SignaturePublicKeyHash)
		prev, _, err := dbGetAnyBlock(cur.PreviousBlockHash)
		if err != nil {
			return nil, fmt.Errorf("The branch is broken: %w", err)
		}
		cur = prev
	}
	slices.Reverse(result)
	return result, nil
}

// acceptBlock validates the block in the given file and adds it to the block tree. It makes the branch of the block the
// best chain if that's what the rules say, and it takes care of the blocks which have been waiting for this one.
func acceptBlock(fileName string, hashSignature []byte) (acceptInfo, error) {
	chainLock.Lock()
	defer chainLock.Unlock()

	info, err := acceptBlockLocked(fileName, hashSignature)
	if err != nil {
		return info, err
	}
	switch {
	case info.result == blockOrphan:
		if err = orphanPool.add(info.hash, info.missing, fileName, hashSignature); err != nil {
			log.Println("Cannot keep the orphan block:", err)
		}
	case info.accepted():
		// The blocks which were waiting for this one (and then, for the ones they have unblocked...)
		queue := []string{info.hash}
		for len(queue) > 0 {
			hash := queue[0]
			queue = queue[1:]
			for _, orphan := range orphanPool.takeChildren(hash) {
				childInfo, err := acceptBlockLocked(orphan.file, orphan.signature)
				if err := os.Remove(orphan.file); err != nil {
					log.Println(err)
				}
				if err != nil {
					log.Println("Cannot import the block", orphan.hash, "which was waiting for", hash+":", err)
					continue
				}
				if childInfo.accepted() {
					queue = append(queue, childInfo.hash)
				}
			}
		}
	}
	return info, nil
}

// Does the work of acceptBlock(). Expects chainLock to be held.
func acceptBlockLocked(fileName string, hashSignature []byte) (info acceptInfo, err error) {
	blk, err := OpenBlockFile(fileName)
	if err != nil {
		return info, err
	}
	defer blk.Close()
	blk.HashSignature = hashSignature
	info.hash = blk.Hash

	if dbBlockHashKnown(blk.Hash) {
		info.result = blockAlreadyKnown
		return info, nil
	}
	if blk.PreviousBlockHash == GenesisBlockPreviousBlockHash {
		return info, errors.New("This is a genesis block, and the genesis block is already known")
	}
	prev, prevOnBestChain, err := dbGetAnyBlock(blk.PreviousBlockHash)
	if errors.Is(err, sql.ErrNoRows) {
		info.result = blockOrphan
		info.missing = blk.PreviousBlockHash
		return info, nil
	}
	if err != nil {
		return info, err
	}
	height := prev.Height + 1
	info.height = height
	tip, err := dbGetTipBlock()
	if err != nil {
		return info, err
	}

	// Validate the block against the keys as they were after the previous block, which may be on another branch
	keys, branch, forkHeight, err := keysAfterBlock(prev, prevOnBestChain)
	if err != nil {
		return info, err
	}
	if tip.Height-forkHeight > maxReorgDepth {
		return info, fmt.Errorf("The block would fork the blockchain %d blocks below the tip, and the limit is %d",
			tip.Height-forkHeight, maxReorgDepth)
	}
	parentTime, err := readBlockTimestamp(blockFileOf(prev, prevOnBestChain))
	if err != nil {
		return info, err
	}
	recent, err := recentSignersOf(prev, recentSignerWindow(keys))
	if err != nil {
		return info, err
	}
	ops, err := validateBlock(blk, height, keys, blockContext{
		genesisHash:   chainParams.GenesisBlockHash,
		parentTime:    parentTime,
		now:           time.Now(),
		blockPeriod:   chainParams.blockPeriod(),
		recentSigners: recent,
	})
	if err != nil {
		return info, err
	}
	// The block is valid and signed by a signatory: only now make sure that its file is not corrupt, because the check
	// reads the whole file (the blocks which are already known, and the orphans, which are checked when their
	// predecessor arrives, don't need that)
	if err = blk.QuickCheck(); err != nil {
		return info, err
	}

	dbb := *blk.DbBlockchainBlock
	dbb.Height = height
	dbb.TimeAccepted = time.Now()
	dbb.TotalScore = prev.TotalScore + blockScore(keys, height, blk.SignaturePublicKeyHash)

	if prevOnBestChain && prev.Height == tip.Height {
		if err = addToBestChain(fileName, &dbb, ops); err != nil {
			return info, err
		}
		log.Println("Accepted block", dbb.Hash, "at height", height)
		info.result = blockNewTip
		return info, nil
	}

	// A block on another branch
	if err = copyFileTo(fileName, sideBlockFilename(dbb.Hash)); err != nil {
		return info, err
	}
	if err = dbInsertBlockInto(mainDb, "sideblocks", &dbb); err != nil {
		os.Remove(sideBlockFilename(dbb.Hash))
		return info, err
	}
	if !isBetterTip(&dbb, tip) {
		log.Printf("Accepted block %s at height %d on a side branch (score %d vs. %d of the best chain)", dbb.Hash, height, dbb.TotalScore, tip.TotalScore)
		info.result = blockOnSideBranch
		return info, nil
	}
	if err = reorganize(forkHeight, append(branch, &dbb), tip); err != nil {
		return info, fmt.Errorf("reorganization failed: %w", err)
	}
	info.result = blockReorganized
	return info, nil
}

// keysAfterBlock returns the signatory keys as they are after the given block, and the way to that block: the blocks
// of the branch which it is on, from the first block after the fork (empty if the block is on the best chain)
// to the block itself, and the height of the best chain block where the branch forks off.
func keysAfterBlock(block *DbBlockchainBlock, onBestChain bool) (keys *keySet, branch []*DbBlockchainBlock, forkHeight int, err error) {
	if keys, err = dbLoadKeySet(); err != nil {
		return nil, nil, 0, err
	}
	if onBestChain {
		keys.rewind(block.Height)
		return keys, nil, block.Height, nil
	}
	cur := block
	for !onBestChain {
		branch = append(branch, cur)
		if cur, onBestChain, err = dbGetAnyBlock(cur.PreviousBlockHash); err != nil {
			return nil, nil, 0, fmt.Errorf("The branch is broken: %w", err)
		}
	}
	forkHeight = cur.Height
	slices.Reverse(branch)
	keys.rewind(forkHeight)
	for _, b := range branch {
		ops, err := readKeyOps(sideBlockFilename(b.Hash))
		if err != nil {
			return nil, nil, 0, err
		}
		keys.apply(b.Height, ops)
	}
	return keys, branch, forkHeight, nil
}

// addToBestChain makes the block the new tip of the best chain
func addToBestChain(fileName string, dbb *DbBlockchainBlock, ops map[string][]BlockKeyOp) error {
	if err := blockchainCopyFile(fileName, dbb.Height); err != nil {
		return err
	}
	err := dbTransaction(func(tx *sql.Tx) error {
		if err := dbInsertBlockInto(tx, "blockchain", dbb); err != nil {
			return err
		}
		return dbApplyKeyOps(tx, dbb.Height, ops)
	})
	if err != nil {
		os.Remove(blockchainGetFilename(dbb.Height))
	}
	return err
}

// Runs f in a database transaction, which is committed if f succeeds
func dbTransaction(f func(tx *sql.Tx) error) error {
	tx, err := mainDb.Begin()
	if err != nil {
		return err
	}
	if err = f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// reorganize makes the given branch, which forks off the best chain after the block at forkHeight, the best chain.
// The blocks of the branch must already be in the sideblocks table. The blocks which it replaces become the side branch.
//
// The database changes are made in one transaction, but the files can't be: the new files are first put next to
// the old ones with the ".new" extension, and renamed after the commit. If the process dies in between,
// recoverReorganization() finishes the job when it's started again.
func reorganize(forkHeight int, branch []*DbBlockchainBlock, oldTip *DbBlockchainBlock) error {
	newTip := branch[len(branch)-1]
	log.Printf("Reorganizing the blockchain: %d block(s) from height %d (score %d) are replaced by %d block(s) from height %d (score %d)",
		oldTip.Height-forkHeight, oldTip.Height, oldTip.TotalScore, len(branch), newTip.Height, newTip.TotalScore)

	// Keep the blocks which are going to be replaced
	var replaced []*DbBlockchainBlock
	for h := forkHeight + 1; h <= oldTip.Height; h++ {
		b, err := dbGetBlockByHeight(h)
		if err != nil {
			return err
		}
		if err = copyFileTo(blockchainGetFilename(h), sideBlockFilename(b.Hash)); err != nil {
			return err
		}
		replaced = append(replaced, b)
	}
	// Prepare the new ones
	ops := make([]map[string][]BlockKeyOp, len(branch))
	for i, b := range branch {
		var err error
		if ops[i], err = readKeyOps(sideBlockFilename(b.Hash)); err != nil {
			return err
		}
		if err = copyFileTo(sideBlockFilename(b.Hash), stagedBlockFilename(b.Height)); err != nil {
			return err
		}
	}

	err := dbTransaction(func(tx *sql.Tx) error {
		for _, b := range replaced {
			if err := dbInsertBlockInto(tx, "sideblocks", b); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM blockchain WHERE height > ?", forkHeight); err != nil {
			return err
		}
		if err := dbRewindKeys(tx, forkHeight); err != nil {
			return err
		}
		for i, b := range branch {
			if _, err := tx.Exec("DELETE FROM sideblocks WHERE hash=?", b.Hash); err != nil {
				return err
			}
			if err := dbInsertBlockInto(tx, "blockchain", b); err != nil {
				return err
			}
			if err := dbApplyKeyOps(tx, b.Height, ops[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		for _, b := range branch {
			os.Remove(stagedBlockFilename(b.Height))
		}
		return err
	}

	for _, b := range branch {
		if err = os.Rename(stagedBlockFilename(b.Height), blockchainGetFilename(b.Height)); err != nil {
			return err // recoverReorganization() will do it next time
		}
	}
	for h := newTip.Height + 1; h <= oldTip.Height; h++ {
		if err = os.Remove(blockchainGetFilename(h)); err != nil {
			log.Println(err)
		}
	}
	log.Println("The blockchain is now at height", newTip.Height, "with the tip", newTip.Hash)
	return nil
}

// recoverReorganization finishes a reorganization which was interrupted: the database has the new blocks, but the
// files of some of them still have their temporary names. Files which don't match the database are removed.
func recoverReorganization() error {
	staged, err := filepath.Glob(filepath.Join(blockchainSubdirectory, "*", "block_*.db.new"))
	if err != nil {
		return err
	}
	for _, fn := range staged {
		var height int
		if _, err := fmt.Sscanf(filepath.Base(fn), "block_%08x.db.new", &height); err != nil {
			continue
		}
		hash, err := util.HashFileToHexString(fn)
		if err == nil {
			if dbb, err := dbGetBlockByHeight(height); err == nil && dbb.Hash == hash {
				log.Println("Completing an interrupted reorganization: block", height)
				if err = os.Rename(fn, blockchainGetFilename(height)); err == nil {
					continue
				}
				log.Println(err)
			}
		}
		if err := os.Remove(fn); err != nil {
			log.Println(err)
		}
	}
	return nil
}

// reconcileChainState fills in what the databases of the older versions didn't have: the heights of the blocks which
// revoked the keys, the heights of the blocks which added the keys of the node which created the chain, and the
// total scores of the blocks. It replays the best chain to find them out.
func reconcileChainState() error {
	if dbGetConfig("schema_version") == fmt.Sprint(currentSchemaVersion) {
		return nil
	}
	log.Println("Upgrading the database: calculating block scores and key operations...")
	keys := newKeySet()
	total := 0
	err := dbTransaction(func(tx *sql.Tx) error {
		maxHeight := dbGetBlockchainHeight()
		for height := 0; height <= maxHeight; height++ {
			dbb, err := dbGetBlockByHeight(height)
			if err != nil {
				return fmt.Errorf("block %d: %w", height, err)
			}
			ops, err := readKeyOps(blockchainGetFilename(height))
			if err != nil {
				return fmt.Errorf("block %d: %w", height, err)
			}
			total += blockScore(keys, height, dbb.SignaturePublicKeyHash)
			if _, err = tx.Exec("UPDATE blockchain SET total_score=? WHERE height=?", total, height); err != nil {
				return err
			}
			if err = dbApplyKeyOps(tx, height, ops); err != nil {
				return err
			}
			keys.apply(height, ops)
		}
		return nil
	})
	if err != nil {
		return err
	}
	dbSetConfig("schema_version", fmt.Sprint(currentSchemaVersion))
	return nil
}

/*
 * Blocks whose predecessor is not known (yet), waiting for it to arrive.
 */

const (
	maxOrphans     = 16
	maxOrphanBytes = 2 << 30
	orphanMaxAge   = 10 * time.Minute
)

type orphanBlock struct {
	hash      string
	prev      string // The hash of the block that's needed
	file      string
	signature []byte
	size      int64
	added     time.Time
}

type orphanPoolType struct {
	lock   sync.Mutex
	blocks map[string]*orphanBlock // By block hash
}

var orphanPool = orphanPoolType{blocks: make(map[string]*orphanBlock)}

// Must be called with the lock held
func (p *orphanPoolType) remove(o *orphanBlock) {
	delete(p.blocks, o.hash)
	if err := os.Remove(o.file); err != nil {
		log.Println(err)
	}
}

// add keeps a copy of the given block file. The blocks are not validated, so there's a limit to how many are kept.
func (p *orphanPoolType) add(hash, prev, fileName string, signature []byte) error {
	st, err := os.Stat(fileName)
	if err != nil {
		return err
	}
	if st.Size() > maxOrphanBytes {
		return fmt.Errorf("the block is too large to wait for its predecessor")
	}
	p.lock.Lock()
	defer p.lock.Unlock()

	for _, o := range p.blocks {
		if time.Since(o.added) > orphanMaxAge {
			p.remove(o)
		}
	}
	if _, ok := p.blocks[hash]; ok {
		return nil
	}
	for {
		var total int64
		var oldest *orphanBlock
		for _, o := range p.blocks {
			total += o.size
			if oldest == nil || o.added.Before(oldest.added) {
				oldest = o
			}
		}
		if oldest == nil || (len(p.blocks) < maxOrphans && total+st.Size() <= maxOrphanBytes) {
			break
		}
		p.remove(oldest)
	}
	dst := filepath.Join(orphanDirname(), hash+".db")
	if err = copyFileTo(fileName, dst); err != nil {
		return err
	}
	p.blocks[hash] = &orphanBlock{hash: hash, prev: prev, file: dst, signature: signature, size: st.Size(), added: time.Now()}
	return nil
}

// takeChildren removes and returns the blocks which are waiting for the given block. The caller removes their files.
func (p *orphanPoolType) takeChildren(prev string) []*orphanBlock {
	p.lock.Lock()
	defer p.lock.Unlock()
	var children []*orphanBlock
	for _, o := range p.blocks {
		if o.prev == prev {
			children = append(children, o)
		}
	}
	for _, o := range children {
		delete(p.blocks, o.hash)
	}
	return children
}
