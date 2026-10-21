package daisy

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

// Tests of the limits on what a peer can make a node do: time, connections, requests, memory and disk.

func TestIncomingBudget(t *testing.T) {
	b := newIncomingBudget(100, 60)
	if !b.reserve("a", 50) || b.reserve("a", 20) {
		t.Fatal("a host can have 60 bytes")
	}
	if !b.reserve("b", 50) || b.reserve("c", 1) {
		t.Fatal("all the hosts together can have 100 bytes")
	}
	b.release("a", 50)
	if !b.reserve("c", 50) {
		t.Fatal("released bytes are available again")
	}
	b.release("a", 0)
	b.release("b", 50)
	b.release("c", 50)
	if b.totalBytes() != 0 || len(b.byHost) != 0 {
		t.Errorf("the budget is not empty: %d bytes, %v", b.totalBytes(), b.byHost)
	}
}

func blockHeader(t *testing.T, b testBlock, mangle func(m map[string]any)) util.StrIfMap {
	t.Helper()
	st, err := os.Stat(b.file)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{
		"hash": b.hash, "hash_signature": hex.EncodeToString(b.sig), "size": float64(st.Size()), "encoding": p2pEncodingChunked,
	}
	if mangle != nil {
		mangle(m)
	}
	return m
}

// Nothing is reserved for a block that can't be valid: not signed by any key that is known, and not asked for
func TestBlockMessageNeedsAKnownKeyOrARequest(t *testing.T) {
	a, stranger, revoked := newTestSigner(t), newTestSigner(t), newTestSigner(t)
	tc := setupTestChain(t, a)
	known := tc.newBlock(tc.genesis.hash, a, "signed by a known key")
	fromStranger := tc.newBlock(tc.genesis.hash, stranger, "signed by a stranger")
	fromOldKey := tc.newBlock(tc.genesis.hash, revoked, "signed by a key of a discarded branch")
	before := p2pIncoming.totalBytes()

	p2pc, _ := newPipeConnection(t, "203.0.113.20:2017")
	defer p2pc.abortAllIncomingBlocks()

	p2pc.handleBlock(blockHeader(t, fromStranger, nil))
	if len(p2pc.incoming) != 0 || p2pIncoming.totalBytes() != before {
		t.Fatal("an unsolicited block signed by a stranger has been received")
	}
	p2pc.handleBlock(blockHeader(t, fromStranger, func(m map[string]any) { m["hash_signature"] = hex.EncodeToString(known.sig) }))
	if len(p2pc.incoming) != 0 {
		t.Fatal("a signature of another block was believed")
	}

	// Asked for: the branch might be the one that adds the key of the signer
	p2pc.noteRequested(fromStranger.hash)
	p2pc.handleBlock(blockHeader(t, fromStranger, nil))
	if len(p2pc.incoming) != 1 {
		t.Fatal("a block which we have asked for has not been accepted")
	}
	p2pc.abortAllIncomingBlocks()
	p2pc.handleBlock(blockHeader(t, fromStranger, nil))
	if len(p2pc.incoming) != 0 {
		t.Fatal("a request can be used only once")
	}

	// Signed by a key we know
	p2pc.handleBlock(blockHeader(t, known, nil))
	if len(p2pc.incoming) != 1 {
		t.Fatal("a block signed by a known key has not been accepted")
	}
	p2pc.abortAllIncomingBlocks()

	// Keys of the discarded branches count too, whatever state they are in
	p2pc.handleBlock(blockHeader(t, fromOldKey, nil))
	if len(p2pc.incoming) != 0 {
		t.Fatal("an unknown key was believed")
	}
	dbWritePublicKey(revoked.pub, revoked.hash, -1)
	p2pc.handleBlock(blockHeader(t, fromOldKey, nil))
	if len(p2pc.incoming) != 1 {
		t.Fatal("a block signed by a key which was only on a discarded branch has not been accepted")
	}
	p2pc.abortAllIncomingBlocks()

	// Garbage
	for name, mangle := range map[string]func(m map[string]any){
		"no size":          func(m map[string]any) { delete(m, "size") },
		"a huge signature": func(m map[string]any) { m["hash_signature"] = hex.EncodeToString(make([]byte, 500)) },
		"an empty hash":    func(m map[string]any) { m["hash"] = "" },
		"a known block":    func(m map[string]any) { m["hash"] = tc.genesis.hash },
	} {
		p2pc.handleBlock(blockHeader(t, known, mangle))
		if len(p2pc.incoming) != 0 {
			t.Errorf("%s: the block has been accepted", name)
		}
	}
	if p2pIncoming.totalBytes() != before {
		t.Error("the budget was not released")
	}
}

func TestBlockWithTheWrongDataIsDropped(t *testing.T) {
	withSmallChunks(t, 1000)
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "the announced block")
	b2 := tc.newBlock(tc.genesis.hash, a, "the block which is sent instead")
	before := p2pIncoming.totalBytes()

	p2pc, _ := newPipeConnection(t, "203.0.113.21:2017")
	// The announcement is that of b1 with its (valid) signature, the data are those of b2
	transferBlockFile(t, p2pc, b1.hash, b1.sig, b2.file, func(i int, m map[string]any) map[string]any {
		m["hash"] = b1.hash
		if i == 0 {
			m["hash_signature"] = hex.EncodeToString(b1.sig)
		}
		return m
	})
	if tc.tip().Height != 0 {
		t.Fatal("a block which isn't the announced one has been accepted")
	}
	if len(p2pc.incoming) != 0 || p2pIncoming.totalBytes() != before {
		t.Error("the transfer was not cleaned up")
	}
	if glob, _ := filepathGlobOrphans(); len(glob) != 0 {
		t.Errorf("something was left in the orphan pool: %v", glob)
	}
}

func TestInboundConnectionsPerHost(t *testing.T) {
	var added []*p2pConnection
	for i, c := range []struct {
		host     string
		outbound bool
	}{{"198.51.100.1", false}, {"198.51.100.1", false}, {"198.51.100.1", true}, {"198.51.100.2", false}} {
		p := newTestConnection(nil, fmt.Sprintf("%s:%d", c.host, 40000+i))
		p.host, p.outbound = c.host, c.outbound
		p2pPeers.Add(p)
		added = append(added, p)
	}
	defer func() {
		for _, p := range added {
			p2pPeers.Remove(p)
		}
	}()
	if n := p2pPeers.CountInboundFrom("198.51.100.1"); n != 2 {
		t.Errorf("%d inbound connections from the host, want 2", n)
	}
	if n := p2pPeers.CountInboundFrom("198.51.100.3"); n != 0 {
		t.Errorf("%d inbound connections from a host with none", n)
	}
}

// reads the lines from the other end of a pipe into a channel, so that writing to it never blocks
func linesFrom(conn net.Conn) chan util.StrIfMap {
	ch := make(chan util.StrIfMap, 1000)
	go func() {
		defer close(ch)
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			msg := util.StrIfMap{}
			if err := json.Unmarshal(line, &msg); err != nil {
				return
			}
			ch <- msg
		}
	}()
	return ch
}

func startConnection(t *testing.T, address string) (remote net.Conn, lines chan util.StrIfMap, done chan struct{}) {
	t.Helper()
	setupTestSysDbs(t)
	local, remote := net.Pipe()
	t.Cleanup(func() { remote.Close() })
	p2pc := newTestConnection(local, address)
	p2pPeers.Add(p2pc)
	done = make(chan struct{})
	go func() {
		p2pc.handleConnection()
		close(done)
	}()
	return remote, linesFrom(remote), done
}

func waitClosed(t *testing.T, done chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatal(what)
	}
}

func TestSilentConnectionIsClosed(t *testing.T) {
	old := p2pHelloTimeout
	p2pHelloTimeout = 200 * time.Millisecond
	defer func() { p2pHelloTimeout = old }()
	remote, lines, done := startConnection(t, "203.0.113.30:2017")
	defer remote.Close()
	if hello := <-lines; hello["msg"] != p2pMsgHello {
		t.Fatalf("expected a hello, got %v", hello)
	}
	// Nothing is sent back, and not even rubbish keeps the connection alive
	for range 3 {
		remote.Write([]byte("{\"root\": \"x\"}\n"))
	}
	waitClosed(t, done, 5*time.Second, "a connection which never says hello is still open")
}

func TestIdleConnectionIsClosedButPingsKeepItOpen(t *testing.T) {
	oldHello, oldIdle, oldPing := p2pHelloTimeout, p2pIdleTimeout, p2pPingInterval
	p2pHelloTimeout, p2pIdleTimeout, p2pPingInterval = 2*time.Second, 400*time.Millisecond, 50*time.Millisecond
	defer func() { p2pHelloTimeout, p2pIdleTimeout, p2pPingInterval = oldHello, oldIdle, oldPing }()
	remote, lines, done := startConnection(t, "203.0.113.31:2017")
	defer remote.Close()
	<-lines // Their hello

	send := func(msg map[string]any) {
		msg["root"] = chainParams.GenesisBlockHash
		msg["p2p_id"] = 77
		remote.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := remote.Write(append(util.JSONifyBytes(msg), '\n')); err != nil {
			t.Fatalf("the connection has been closed: %v", err)
		}
	}
	send(map[string]any{"msg": p2pMsgHello, "version": "test", "chain_height": 0, "my_peers": []string{},
		"protocol": p2pProtocolVersion, "min_protocol": p2pMinProtocolVersion})

	// Longer than the idle timeout, with only pings
	pingsReceived := 0
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		send(map[string]any{"msg": p2pMsgPing})
		time.Sleep(100 * time.Millisecond)
	}
drain:
	for {
		select {
		case m := <-lines:
			if m["msg"] == p2pMsgPing {
				pingsReceived++
			}
		default:
			break drain
		}
	}
	select {
	case <-done:
		t.Fatal("the connection was closed although the peer was sending pings")
	default:
	}
	if pingsReceived == 0 {
		t.Error("the node doesn't ping its peers")
	}
	// ... but it is closed when nothing arrives
	waitClosed(t, done, 5*time.Second, "an idle connection is still open")
}

func TestSecondHelloIsIgnored(t *testing.T) {
	setupTestSysDbs(t)
	drainCtrl()
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	p2pc := newTestConnection(&fakeAddrConn{Conn: local, remote: "192.0.2.50:40000"}, "192.0.2.50:40000")

	hello := helloMsg(5001, 3, "", 0)
	hello["my_peers"] = []any{"198.51.100.77"}
	hello["observed_host"] = "198.51.100.1"
	p2pc.handleMsgHello(hello)
	connectRequests := func() int {
		n := 0
		for _, m := range drainCtrl() {
			if m.msgType == p2pCtrlConnectPeers {
				n++
			}
		}
		return n
	}
	if !p2pc.handshaken || connectRequests() != 1 {
		t.Fatal("the first hello was not processed")
	}
	selfInfo.lock.With(func() { selfInfo.observed = make(map[string]map[string]bool) })

	second := helloMsg(5002, 99, "", 0)
	second["my_peers"] = []any{"198.51.100.78"}
	second["observed_host"] = "198.51.100.2"
	second["protocol"], second["min_protocol"] = float64(p2pProtocolVersion+1), float64(p2pProtocolVersion+1)
	p2pc.handleMsgHello(second)
	if connectRequests() != 0 {
		t.Error("the peers of the second hello were taken")
	}
	if len(selfInfo.observed) != 0 {
		t.Error("the second hello has voted about our address")
	}
	if p2pc.chainHeight != 3 || p2pc.peerID != 5001 || p2pc.protocol.Load() != p2pProtocolVersion {
		t.Errorf("the second hello has changed the connection: height %d, id %d, protocol %d", p2pc.chainHeight, p2pc.peerID, p2pc.protocol.Load())
	}
}
