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
		{"an old node", 2, 1, false},
		{"a node which says nothing speaks protocol 1", 1, 1, false},
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
	// A hello without the version fields is from the original protocol, which is not supported any more
	legacy, _ := newPipeConnection(t, "203.0.113.1:2017")
	hello0 := helloMsg(3001, 0, "", 0)
	delete(hello0, "protocol")
	delete(hello0, "min_protocol")
	legacy.handleMsgHello(hello0)
	if legacy.handshaken {
		t.Error("a node of the original protocol has been accepted")
	}
	// ... and one with the fields
	current, _ := newPipeConnection(t, "203.0.113.2:2017")
	hello := helloMsg(3002, 0, "", 0)
	hello["protocol"], hello["min_protocol"] = float64(p2pProtocolVersion), float64(p2pMinProtocolVersion)
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
	sender := newBlockSender(hash, sig, file)
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
	before := p2pIncoming.totalBytes()
	transferBlockFile(t, p2pc, b1.hash, b1.sig, b1.file, nil)
	if tc.tip().Hash != b1.hash {
		t.Fatal("the block that was sent in chunks has not been accepted")
	}
	if len(p2pc.incoming) != 0 || p2pIncoming.totalBytes() != before {
		t.Errorf("the transfer was not cleaned up: %d transfers, %d bytes", len(p2pc.incoming), p2pIncoming.totalBytes()-before)
	}
	tc.checkConsistent()
}

func TestBlockTransferAbuse(t *testing.T) {
	withSmallChunks(t, 1000)
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "abused")
	before := p2pIncoming.totalBytes()

	try := func(name string, mangle func(i int, m map[string]any) map[string]any) {
		t.Helper()
		p2pc, _ := newPipeConnection(t, "203.0.113.11:2017")
		transferBlockFile(t, p2pc, b1.hash, b1.sig, b1.file, mangle)
		if tc.tip().Height != 0 {
			t.Fatalf("%s: the block has been accepted", name)
		}
		if len(p2pc.incoming) != 0 || p2pIncoming.totalBytes() != before {
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
	// The signature isn't one of a key that we know: nothing is even started
	try("a wrong signature", func(i int, m map[string]any) map[string]any {
		if i == 0 {
			m["hash_signature"] = strings.Repeat("00", 70)
		}
		return m
	})
	try("a signature that is not hex", func(i int, m map[string]any) map[string]any {
		if i == 0 {
			m["hash_signature"] = "zz"
		}
		return m
	})
	try("a hash that is not a hash", func(i int, m map[string]any) map[string]any {
		if i == 0 {
			m["hash"] = "hello"
		}
		return m
	})
	try("an encoding of the old protocol", func(i int, m map[string]any) map[string]any {
		if i == 0 {
			m["encoding"], m["data"] = "zlib-base64", "AAAA"
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
	old := p2pIncoming
	defer func() { p2pIncoming = old }()
	p2pIncoming = newIncomingBudget(10000, 3000)

	p2pc, _ := newPipeConnection(t, "203.0.113.13:2017")
	for i := 0; i < p2pMaxIncomingTransfers+3; i++ {
		p2pc.startIncomingBlock(fmt.Sprintf("hash%d", i), "00", 1000)
	}
	if len(p2pc.incoming) != p2pMaxIncomingTransfers {
		t.Errorf("%d transfers at the same time, the limit is %d", len(p2pc.incoming), p2pMaxIncomingTransfers)
	}
	p2pc.abortAllIncomingBlocks()
	if p2pIncoming.totalBytes() != 0 || p2pIncoming.hostBytes("203.0.113.13") != 0 {
		t.Fatal("the budget was not released")
	}

	// One host can't have more than its share, whatever the number of its connections
	var conns []*p2pConnection
	for i := range 4 {
		c, _ := newPipeConnection(t, fmt.Sprintf("203.0.113.14:%d", 40000+i))
		conns = append(conns, c)
		c.startIncomingBlock(fmt.Sprintf("host-hash%d", i), "00", 1000)
		defer c.abortAllIncomingBlocks()
	}
	got := 0
	for _, c := range conns {
		got += len(c.incoming)
	}
	if got != 3 || p2pIncoming.hostBytes("203.0.113.14") != 3000 {
		t.Errorf("%d transfers from one host, %d bytes", got, p2pIncoming.hostBytes("203.0.113.14"))
	}
	// ... and the others still have room
	other, _ := newPipeConnection(t, "203.0.113.15:2017")
	other.startIncomingBlock("other", "00", 1000)
	defer other.abortAllIncomingBlocks()
	if len(other.incoming) != 1 {
		t.Error("another host has been shut out by the first one")
	}

	// The total is limited too
	big, _ := newPipeConnection(t, "203.0.113.16:2017")
	big.startIncomingBlock("too much", "00", 7000) // Over the share of the host
	if len(big.incoming) != 0 {
		t.Error("a transfer bigger than the share of the host started")
	}
	p2pIncoming = newIncomingBudget(1500, 1000)
	a, _ := newPipeConnection(t, "203.0.113.17:2017")
	b, _ := newPipeConnection(t, "203.0.113.18:2017")
	a.startIncomingBlock("a", "00", 1000)
	b.startIncomingBlock("b", "00", 1000)
	defer a.abortAllIncomingBlocks()
	if len(a.incoming) != 1 || len(b.incoming) != 0 {
		t.Error("the total budget is not respected")
	}

	// A transfer which has stalled is dropped
	p2pIncoming = newIncomingBudget(10000, 3000)
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

	p2pc.send(newBlockSender(b1.hash, b1.sig, b1.file))
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
