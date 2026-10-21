package daisy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

func TestProtocolVersionCheck(t *testing.T) {
	for _, tt := range []struct {
		name          string
		protocol, min int
		ok            bool
	}{
		{"the same", p2pProtocolVersion, p2pMinProtocolVersion, true},
		{"an old node", 1, 1, true},
		{"a newer node which still talks to us", p2pProtocolVersion + 5, p2pProtocolVersion, true},
		{"a newer node which doesn't talk to old ones", p2pProtocolVersion + 5, p2pProtocolVersion + 1, false},
		{"a node too old for us", p2pMinProtocolVersion - 1, 1, false},
	} {
		if err := p2pCheckProtocol(tt.protocol, tt.min); (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

func TestHelloVersions(t *testing.T) {
	setupTestSysDbs(t)
	// A hello without the version fields is from the original protocol
	legacy, _ := newPipeConnection(t, "203.0.113.1:2017")
	legacy.handleMsgHello(helloMsg(3001, 0, "", 0))
	if !legacy.handshaken || legacy.protocol.Load() != 1 {
		t.Errorf("legacy: handshaken=%v protocol=%d", legacy.handshaken, legacy.protocol.Load())
	}
	// ... and one with them
	current, _ := newPipeConnection(t, "203.0.113.2:2017")
	hello := helloMsg(3002, 0, "", 0)
	hello["protocol"], hello["min_protocol"] = float64(p2pProtocolVersion), float64(1)
	current.handleMsgHello(hello)
	if !current.handshaken || current.protocol.Load() != p2pProtocolVersion {
		t.Errorf("current: handshaken=%v protocol=%d", current.handshaken, current.protocol.Load())
	}
	// A node which wants a newer protocol than ours is dropped, and it's not dialed again for a while
	future, remote := newPipeConnection(t, "203.0.113.3:2017")
	future.outbound = true
	hello = helloMsg(3003, 0, "", 0)
	hello["protocol"], hello["min_protocol"] = float64(p2pProtocolVersion+1), float64(p2pProtocolVersion+1)
	future.handleMsgHello(hello)
	if future.handshaken {
		t.Error("an incompatible peer has been accepted")
	}
	if !p2pCoordinator.badPeers.Has("203.0.113.3:2017") {
		t.Error("an incompatible peer which we have dialed should not be dialed again")
	}
	remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Read(make([]byte, 1)); err == nil {
		t.Error("the connection should have been closed")
	}
}

// Sends the block in the file the way the sender does, and delivers it to p2pc the way the receiver gets it
func transferBlockFile(t *testing.T, p2pc *p2pConnection, hash string, sig []byte, file string, mangle func(i int, msg map[string]any) map[string]any) {
	t.Helper()
	sender, err := newBlockSender(hash, sig, file)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.close()
	for i := 0; ; i++ {
		msg, done, err := sender.nextMessage()
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(util.JSONifyBytes(msg), &m); err != nil {
			t.Fatal(err)
		}
		if mangle != nil {
			if m = mangle(i, m); m == nil {
				continue // Dropped
			}
		}
		switch m["msg"] {
		case p2pMsgBlock:
			p2pc.handleBlock(m)
		case p2pMsgBlockChunk:
			p2pc.handleBlockChunk(m)
		}
		if done {
			return
		}
	}
}

func withSmallChunks(t *testing.T, size int) {
	old := p2pChunkSize
	p2pChunkSize = size
	t.Cleanup(func() { p2pChunkSize = old })
}

func TestBlockTransferInChunks(t *testing.T) {
	withSmallChunks(t, 1000)
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "sent in pieces")
	st, _ := os.Stat(b1.file)
	if st.Size() < 5000 {
		t.Fatalf("the test block is too small to be sent in several chunks: %d", st.Size())
	}

	p2pc, _ := newPipeConnection(t, "203.0.113.10:2017")
	before := p2pIncomingBytes.Load()
	transferBlockFile(t, p2pc, b1.hash, b1.sig, b1.file, nil)
	if tc.tip().Hash != b1.hash {
		t.Fatal("the block that was sent in chunks has not been accepted")
	}
	if len(p2pc.incoming) != 0 || p2pIncomingBytes.Load() != before {
		t.Errorf("the transfer was not cleaned up: %d transfers, %d bytes", len(p2pc.incoming), p2pIncomingBytes.Load()-before)
	}
	tc.checkConsistent()
}

func TestBlockTransferAbuse(t *testing.T) {
	withSmallChunks(t, 1000)
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "abused")
	before := p2pIncomingBytes.Load()

	try := func(name string, mangle func(i int, m map[string]any) map[string]any) {
		t.Helper()
		p2pc, _ := newPipeConnection(t, "203.0.113.11:2017")
		transferBlockFile(t, p2pc, b1.hash, b1.sig, b1.file, mangle)
		if tc.tip().Height != 0 {
			t.Fatalf("%s: the block has been accepted", name)
		}
		if len(p2pc.incoming) != 0 || p2pIncomingBytes.Load() != before {
			t.Errorf("%s: the transfer was not cleaned up", name)
		}
	}
	try("a chunk is missing", func(i int, m map[string]any) map[string]any {
		if i == 2 {
			return nil
		}
		return m
	})
	try("the chunks are in the wrong order", func(i int, m map[string]any) map[string]any {
		if i == 2 {
			m["index"] = float64(5)
		}
		return m
	})
	try("a chunk is not compressed data", func(i int, m map[string]any) map[string]any {
		if i == 2 {
			m["data"] = "bm90IHpsaWI="
		}
		return m
	})
	try("a chunk is not base64", func(i int, m map[string]any) map[string]any {
		if i == 2 {
			m["data"] = "!!!"
		}
		return m
	})
	try("the block is bigger than announced", func(i int, m map[string]any) map[string]any {
		if i == 0 {
			m["size"] = float64(3000)
		}
		return m
	})
	try("a block of an unacceptable size is announced", func(i int, m map[string]any) map[string]any {
		if i == 0 {
			m["size"] = float64(p2pMaxBlockSize + 1)
		}
		return m
	})
	// The data are fine, but they are not the block which is announced... it's only the signature that decides
	try("a wrong signature", func(i int, m map[string]any) map[string]any {
		if i == 0 {
			m["hash_signature"] = strings.Repeat("00", 70)
		}
		return m
	})
	// Chunks for a block that wasn't announced are ignored
	p2pc, _ := newPipeConnection(t, "203.0.113.12:2017")
	p2pc.handleBlockChunk(util.StrIfMap{"hash": "unknown", "index": float64(0), "data": "AAAA"})
	if len(p2pc.incoming) != 0 {
		t.Error("an unannounced chunk started something")
	}
}

func TestTransferLimits(t *testing.T) {
	p2pc, _ := newPipeConnection(t, "203.0.113.13:2017")
	before := p2pIncomingBytes.Load()
	for i := 0; i < p2pMaxIncomingTransfers+3; i++ {
		p2pc.startIncomingBlock(fmt.Sprintf("hash%d", i), "00", 1000)
	}
	if len(p2pc.incoming) != p2pMaxIncomingTransfers {
		t.Errorf("%d transfers at the same time, the limit is %d", len(p2pc.incoming), p2pMaxIncomingTransfers)
	}
	// The budget is for all the connections
	p2pc.abortAllIncomingBlocks()
	if p2pIncomingBytes.Load() != before {
		t.Fatal("the budget was not released")
	}
	p2pIncomingBytes.Add(p2pMaxIncomingBytes - 500)
	defer p2pIncomingBytes.Add(-(p2pMaxIncomingBytes - 500))
	p2pc.startIncomingBlock("too much", "00", 1000)
	if len(p2pc.incoming) != 0 {
		t.Error("a transfer started although there is not enough room")
	}
	// A transfer which has stalled is dropped
	p2pc.startIncomingBlock("stalled", "00", 100)
	p2pc.incoming["stalled"].lastActivity = time.Now().Add(-2 * p2pTransferTimeout)
	p2pc.expireIncomingBlocks()
	if len(p2pc.incoming) != 0 {
		t.Error("the stalled transfer is still there")
	}
}

// The big block doesn't block the messages which are queued after it
func TestOutboxInterleavesBlocksAndMessages(t *testing.T) {
	withSmallChunks(t, 1000)
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "interleaved")

	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	p2pc := newTestConnection(local, "203.0.113.14:2017")
	p2pc.peer = bufio.NewReadWriter(bufio.NewReader(local), bufio.NewWriter(local))
	lines := make(chan string, 100)
	go func() {
		r := bufio.NewReader(remote)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- line
		}
	}()

	sender, err := newBlockSender(b1.hash, b1.sig, b1.file)
	if err != nil {
		t.Fatal(err)
	}
	p2pc.send(sender)
	p2pc.send(map[string]string{"msg": "queued-with-the-block"})
	flushed := 0
	for range 100 {
		if err := p2pc.flushOutbox(); err != nil {
			t.Fatal(err)
		}
		flushed++
		if flushed == 3 {
			// Something that comes up while the block is being sent doesn't wait for the whole block
			p2pc.send(map[string]string{"msg": "queued-during-the-block"})
		}
		select {
		case <-p2pc.outboxReady:
			continue // There's more to send
		default:
		}
		break
	}
	local.Close()

	var kinds []string
	for line := range lines {
		var m map[string]any
		json.Unmarshal([]byte(line), &m)
		kinds = append(kinds, fmt.Sprint(m["msg"]))
	}
	if len(kinds) < 8 {
		t.Fatalf("the block was sent in %d messages: %v", len(kinds), kinds)
	}
	pos := func(kind string) int {
		for i, k := range kinds {
			if k == kind {
				return i
			}
		}
		return -1
	}
	if kinds[0] != "queued-with-the-block" || kinds[1] != p2pMsgBlock {
		t.Errorf("unexpected start: %v", kinds[:3])
	}
	if during := pos("queued-during-the-block"); during < 0 || during > 6 {
		t.Errorf("the message queued during the transfer was sent at position %d of %d", during, len(kinds))
	}
	if kinds[len(kinds)-1] != p2pMsgBlockChunk {
		t.Errorf("the block is not complete: %v", kinds)
	}
}

func TestGetBlockUsesChunksForNewPeersAndHTTPForOld(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "1")
	tc.mustAccept(b1, blockNewTip)

	modern, _ := newPipeConnection(t, "203.0.113.15:2017")
	modern.protocol.Store(2)
	modern.handleGetBlock(util.StrIfMap{"hash": b1.hash})
	if len(modern.outbox) != 1 {
		t.Fatalf("%d messages", len(modern.outbox))
	}
	if _, ok := modern.outbox[0].(*blockSender); !ok {
		t.Errorf("a protocol 2 peer should get the block in chunks, got %T", modern.outbox[0])
	}
	modern.discardOutbox()

	old, _ := newPipeConnection(t, "203.0.113.16:2017")
	old.protocol.Store(1)
	old.handleGetBlock(util.StrIfMap{"hash": b1.hash})
	msg, ok := old.outbox[0].(p2pMsgBlockStruct)
	if !ok || msg.Encoding != "http" {
		t.Errorf("a protocol 1 peer should be told where to download the block, got %T %+v", old.outbox[0], old.outbox[0])
	}
}

func TestSelfInfo(t *testing.T) {
	s := &selfInfoType{observed: make(map[string]map[string]bool), reachable: make(map[string]bool)}
	s.observe("198.51.100.7", "peer1")
	if s.seenAs() != "" || s.isSelf("198.51.100.7") {
		t.Error("one peer is not enough to believe")
	}
	s.observe("198.51.100.7", "peer1") // The same peer again
	if s.seenAs() != "" {
		t.Error("the same peer counted twice")
	}
	s.observe("198.51.100.7", "peer2")
	if s.seenAs() != "198.51.100.7" || !s.isSelf("198.51.100.7") {
		t.Error("two peers agree")
	}
	s.observe("not an address", "peer3")
	s.observe("203.0.113.99", "peer3") // A peer which disagrees
	if s.isSelf("203.0.113.99") {
		t.Error("a single peer's claim was believed")
	}
	// The data kept about the peers is bounded
	for i := 0; i < 100; i++ {
		s.observe(fmt.Sprintf("10.0.0.%d", i), "peer1")
	}
	if len(s.observed) > selfInfoMaxHosts {
		t.Errorf("%d addresses are remembered", len(s.observed))
	}
	if s.isReachable() {
		t.Error("nobody has connected to us")
	}
	s.confirmReachable("peer1")
	s.confirmReachable("peer1")
	if s.isReachable() {
		t.Error("one peer is not enough")
	}
	s.confirmReachable("peer2")
	if !s.isReachable() {
		t.Error("two peers have connected to us")
	}
	s.setExternal("192.0.2.5")
	if !s.isSelf("192.0.2.5") {
		t.Error("the address of the router is ours")
	}
	var nilInfo *selfInfoType
	if nilInfo.isSelf("1.2.3.4") {
		t.Error("nil")
	}
}

func TestReachableMessage(t *testing.T) {
	selfInfo.lock.With(func() { selfInfo.reachable = make(map[string]bool) })
	defer selfInfo.lock.With(func() { selfInfo.reachable = make(map[string]bool) })
	for _, host := range []string{"192.0.2.1", "192.0.2.2"} {
		local, remote := net.Pipe()
		defer local.Close()
		defer remote.Close()
		p2pc := newTestConnection(&fakeAddrConn{Conn: local, remote: host + ":40000"}, host+":40000")
		p2pc.handleReachable(util.StrIfMap{"port": float64(DefaultP2PPort)})
	}
	if !selfInfo.isReachable() {
		t.Error("two peers have said that they have connected to us")
	}
	// A port which is not the one that peers look for doesn't count
	selfInfo.lock.With(func() { selfInfo.reachable = make(map[string]bool) })
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	p2pc := newTestConnection(&fakeAddrConn{Conn: local, remote: "192.0.2.3:40000"}, "192.0.2.3:40000")
	p2pc.handleReachable(util.StrIfMap{"port": float64(1234)})
	if len(selfInfo.reachable) != 0 {
		t.Error("the wrong port was counted")
	}
}

// A connection which has the remote address of our choice
type fakeAddrConn struct {
	net.Conn
	remote string
}

func (c *fakeAddrConn) RemoteAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", c.remote)
	return a
}

var _ = filepath.Join
