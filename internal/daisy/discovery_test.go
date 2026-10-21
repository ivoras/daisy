package daisy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

func drainCtrl() []p2pCtrlMessage {
	var msgs []p2pCtrlMessage
	for {
		select {
		case m := <-p2pCtrlChannel:
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

func helloMsg(id int, height int, tipHash string, score int) util.StrIfMap {
	m := util.StrIfMap{"version": "test", "chain_height": float64(height), "p2p_id": float64(id), "my_peers": []any{},
		"protocol": float64(p2pProtocolVersion), "min_protocol": float64(p2pMinProtocolVersion)}
	if tipHash != "" {
		m["tip_hash"] = tipHash
		m["chain_score"] = float64(score)
	}
	return m
}

func newPipeConnection(t *testing.T, address string) (*p2pConnection, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { local.Close(); remote.Close() })
	return newTestConnection(local, address), remote
}

// Whether the peer's hello makes us look for its blocks
func TestHelloTriggersSearchForBetterChains(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	prev := tc.genesis.hash
	for i := 1; i <= 2; i++ {
		b := tc.newBlock(prev, a, fmt.Sprint(i))
		tc.mustAccept(b, blockNewTip)
		prev = b.hash
	}
	tip := tc.tip()
	other := strings.Repeat("f", 64)
	lowerHash := strings.Repeat("0", 64)

	for i, tt := range []struct {
		name    string
		height  int
		tipHash string
		score   int
		search  bool
	}{
		{"higher, from an old version", 5, "", 0, true},
		{"the same height, from an old version", 2, "", 0, false},
		{"lower, from an old version", 1, "", 0, false},
		{"the same tip", 2, tip.Hash, tip.TotalScore, false},
		{"a different tip with a higher score at the same height", 2, other, tip.TotalScore + 1, true},
		{"a different tip with a higher score, but lower", 1, other, tip.TotalScore + 1, true},
		{"a different tip with a lower score", 2, other, tip.TotalScore - 1, false},
		{"the same score, a lower hash", 2, lowerHash, tip.TotalScore, true},
		{"the same score, a higher hash", 2, other, tip.TotalScore, false},
	} {
		drainCtrl()
		p2pc, _ := newPipeConnection(t, fmt.Sprintf("203.0.113.%d:2017", i+1))
		p2pc.handleMsgHello(helloMsg(1000+i, tt.height, tt.tipHash, tt.score))
		msgs := drainCtrl()
		searched := false
		for _, m := range msgs {
			if m.msgType == p2pCtrlSearchForBlocks {
				searched = true
				if req := m.payload.(p2pSearchRequest); req.peer != p2pc || req.peerHeight != tt.height {
					t.Errorf("%s: unexpected search request %+v", tt.name, req)
				}
			}
		}
		if searched != tt.search {
			t.Errorf("%s: search = %v, want %v", tt.name, searched, tt.search)
		}
	}
}

func TestBlockHashesRequestsTheBlocksOfOtherBranches(t *testing.T) {
	signers := []*testSigner{newTestSigner(t), newTestSigner(t)}
	tc := setupTestChain(t, signers...)
	genesis := tc.genesis.hash
	best := tc.newBlock(genesis, tc.inTurn(genesis), "best")
	side := tc.newBlock(genesis, tc.outOfTurn(genesis), "side")
	tc.mustAccept(best, blockNewTip)
	tc.mustAccept(side, blockOnSideBranch)
	best2 := tc.newBlock(best.hash, tc.inTurn(best.hash), "best2")
	tc.mustAccept(best2, blockNewTip)

	unknown1 := testHash(fmt.Sprintf("unknown-%d-1", time.Now().UnixNano()))
	unknown2 := testHash(fmt.Sprintf("unknown-%d-2", time.Now().UnixNano()))
	veryOld := testHash(fmt.Sprintf("old-%d", time.Now().UnixNano()))

	p2pc, _ := newPipeConnection(t, "203.0.113.50:2017")
	p2pc.handleBlockHashes(util.StrIfMap{"hashes": map[string]any{
		"0":    genesis,    // Known, best chain
		"1":    unknown1,   // A competing block at a height where we have a block
		"2":    best2.hash, // Known
		"3":    unknown2,   // Beyond our tip
		"-300": veryOld,    // Too far below the tip to ever be accepted
		"4":    side.hash,  // Known, on a side branch
	}})
	var requested []string
	for _, m := range p2pc.outbox {
		req := m.(p2pMsgGetBlockStruct)
		requested = append(requested, req.Hash)
	}
	if len(requested) != 2 || requested[0] != unknown1 || requested[1] != unknown2 {
		t.Errorf("requested %v, want [%s %s]", requested, unknown1, unknown2)
	}
}

func TestSideBlocksAreServed(t *testing.T) {
	signers := []*testSigner{newTestSigner(t), newTestSigner(t)}
	tc := setupTestChain(t, signers...)
	genesis := tc.genesis.hash
	side := tc.newBlock(genesis, tc.outOfTurn(genesis), "side")
	best := tc.newBlock(genesis, tc.inTurn(genesis), "best")
	tc.mustAccept(side, blockNewTip)
	tc.mustAccept(best, blockReorganized) // The side block was on the best chain, and it's moved

	conn, _ := newPipeConnection(t, "203.0.113.60:2017")
	conn.handleGetBlock(util.StrIfMap{"hash": side.hash})
	if len(conn.outbox) != 1 {
		t.Fatalf("expected a block sender, got %d messages", len(conn.outbox))
	}
	sender := conn.outbox[0].(*blockSender)
	defer sender.close()
	first, _, err := sender.nextMessage()
	if err != nil {
		t.Fatal(err)
	}
	msg := first.(p2pMsgBlockStruct)
	if msg.Hash != side.hash || msg.Encoding != p2pEncodingChunked {
		t.Errorf("unexpected block message %+v", msg)
	}
	if st, err := os.Stat(side.file); err != nil || msg.Size != st.Size() {
		t.Errorf("announced size %d", msg.Size)
	}

	// The same through HTTP
	mux := http.NewServeMux()
	mux.HandleFunc("GET /block/{id}", blockWebSendBlock)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(id string) (int, string) {
		resp, err := http.Get(srv.URL + "/block/" + id)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, util.HashBytesToHexString(body)
	}
	for id, want := range map[string]string{side.hash: side.hash, best.hash: best.hash, "1": best.hash, "0": genesis} {
		if status, hash := get(id); status != 200 || hash != want {
			t.Errorf("/block/%s: status %d, hash %s, want %s", id, status, hash, want)
		}
	}
	for id, want := range map[string]int{"99": 404, strings.Repeat("a", 64): 404, "zzz": 400, "-1": 400} {
		if status, _ := get(id); status != want {
			t.Errorf("/block/%s: status %d, want %d", id, status, want)
		}
	}
}

// Nothing but hello is accepted from a peer which hasn't said hello
func TestNoMessagesBeforeHello(t *testing.T) {
	setupTestSysDbs(t)
	local, remote := net.Pipe()
	defer remote.Close()
	p2pc := newTestConnection(local, "203.0.113.70:2017")
	p2pPeers.Add(p2pc)
	done := make(chan struct{})
	go func() { p2pc.handleConnection(); close(done) }()

	in := bufio.NewReader(remote)
	readJSONLine(t, in) // Their hello
	announce := util.JSONifyBytes(map[string]any{
		"root": chainParams.GenesisBlockHash, "msg": p2pMsgBlockHashes, "p2p_id": 2,
		"hashes": map[string]string{"1": "ignored-before-hello"},
	})
	remote.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := remote.Write(append(announce, '\n')); err != nil {
		t.Fatal(err)
	}
	remote.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if line, err := in.ReadBytes('\n'); err == nil {
		t.Fatalf("got a reply to a message sent before hello: %s", line)
	}
	remote.Close()
	<-done
}

func TestHelloSavesOnlyOutboundPeers(t *testing.T) {
	setupTestSysDbs(t)
	count := func() int {
		var n int
		mainDb.QueryRow("SELECT COUNT(*) FROM peers").Scan(&n)
		return n
	}
	inbound, _ := newPipeConnection(t, "203.0.113.80:40000")
	inbound.handleMsgHello(helloMsg(2001, 0, "", 0))
	if count() != 0 {
		t.Error("a peer which connected to us has been saved")
	}
	if !inbound.handshaken {
		t.Error("the hello wasn't recorded")
	}
	outbound, _ := newPipeConnection(t, "203.0.113.81:2017")
	outbound.outbound = true
	outbound.savedAddress = "peers.example.com:2017"
	outbound.handleMsgHello(helloMsg(2002, 0, "", 0))
	var address string
	if err := mainDb.QueryRow("SELECT address FROM peers").Scan(&address); err != nil || address != "peers.example.com:2017" {
		t.Errorf("saved %q, %v", address, err)
	}
	if !outbound.isConnectable {
		t.Error("a peer which we have connected to on the default port should be known to be connectable")
	}
}

func TestPeerSetHandshakeAndConnectability(t *testing.T) {
	set := &p2pPeersSet{peers: make(map[*p2pConnection]time.Time)}
	mk := func(address string, outbound bool) *p2pConnection {
		c := &p2pConnection{address: address, outbound: outbound}
		set.Add(c)
		return c
	}
	defaultPort := mk("198.51.100.1:2017", true)
	otherPort := mk("198.51.100.2:3000", true)
	inbound := mk("198.51.100.3:50000", false)
	silent := mk("198.51.100.4:2017", true) // Never says hello
	inbound.isConnectable = true            // The test connection back has succeeded
	silent.isConnectable = true

	if got := set.GetAddresses(true); len(got) != 0 {
		t.Errorf("nobody has said hello, but %v are advertised", got)
	}
	for _, c := range []*p2pConnection{defaultPort, otherPort, inbound} {
		set.markHandshaken(c)
	}
	got := set.GetAddresses(true)
	if len(got) != 2 || !(got[0] == "198.51.100.1:2017" || got[1] == "198.51.100.1:2017") {
		t.Errorf("advertised %v, want the outbound default port peer and the inbound connectable one", got)
	}
	if otherPort.isConnectable {
		t.Error("a peer on another port is not known to be connectable")
	}
	if n := set.Count(true); n != 3 {
		t.Errorf("%d outbound connections", n)
	}
	if n := set.Count(false); n != 1 {
		t.Errorf("%d inbound connections", n)
	}
	if n := len(set.Snapshot(true)); n != 3 {
		t.Errorf("%d peers have said hello", n)
	}
}

func TestSaveConnectablePeers(t *testing.T) {
	setupTestSysDbs(t)
	c := &p2pConnection{address: "203.0.113.90:49152", isConnectable: true, handshaken: true}
	silent := &p2pConnection{address: "203.0.113.91:49152", isConnectable: true}
	p2pPeers.Add(c)
	p2pPeers.Add(silent)
	defer p2pPeers.Remove(c)
	defer p2pPeers.Remove(silent)

	p2pPeers.saveConnectablePeers()
	saved := dbGetSavedPeers()
	if _, ok := saved["203.0.113.90:2017"]; !ok || len(saved) != 1 {
		t.Errorf("saved peers: %v", saved)
	}
}
