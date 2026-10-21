package daisy

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

// Gives the test empty system databases, which is all the p2p code under test needs.
func setupTestSysDbs(t *testing.T) {
	t.Helper()
	var err error
	dir := t.TempDir()
	if mainDb, err = sql.Open("sqlite3", filepath.Join(dir, "main.db")); err != nil {
		t.Fatal(err)
	}
	if _, err = mainDb.Exec(blockchainTableCreate); err != nil {
		t.Fatal(err)
	}
	if privateDb, err = sql.Open("sqlite3", filepath.Join(dir, "private.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mainDb.Close()
		privateDb.Close()
		mainDb, privateDb = nil, nil
	})
}

func newTestConnection(conn net.Conn, address string) *p2pConnection {
	return &p2pConnection{
		conn:         conn,
		address:      address,
		chanFromPeer: make(chan util.StrIfMap, 5),
		outboxReady:  make(chan struct{}, 1),
	}
}

func readJSONLine(t *testing.T, r *bufio.Reader) util.StrIfMap {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading a message from the connection: %v", err)
	}
	var msg util.StrIfMap
	if err = json.Unmarshal(line, &msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

// Regression test: a connection used to deadlock on itself when a peer announced more than 5 blocks
// we don't have, because the handler queued the "getblock" requests into a fixed-size channel that only
// the (blocked) handler loop itself could drain.
func TestConnectionRequestsManyBlocks(t *testing.T) {
	setupTestSysDbs(t)
	const numBlocks = 20

	local, remote := net.Pipe()
	defer remote.Close()
	p2pc := newTestConnection(local, "10.0.0.1:2017")
	p2pPeers.Add(p2pc)
	done := make(chan struct{})
	go func() {
		p2pc.handleConnection()
		close(done)
	}()

	remoteIn := bufio.NewReader(remote)
	if hello := readJSONLine(t, remoteIn); hello["msg"] != p2pMsgHello {
		t.Fatalf("expected a hello message, got %v", hello)
	}

	// The hashes must be unique per run: recently requested ones are (rightly) not requested again
	hashes := map[string]string{}
	for h := 1; h <= numBlocks; h++ {
		hashes[fmt.Sprint(h)] = fmt.Sprintf("test-many-blocks-%d-%d", time.Now().UnixNano(), h)
	}
	announce := util.JSONifyBytes(map[string]any{
		"root": chainParams.GenesisBlockHash, "msg": p2pMsgBlockHashes, "p2p_id": 1, "hashes": hashes,
	})
	remote.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := remote.Write(append(announce, '\n')); err != nil {
		t.Fatal(err)
	}

	for h := 1; h <= numBlocks; h++ {
		req := readJSONLine(t, remoteIn)
		if req["msg"] != p2pMsgGetBlock || req["hash"] != hashes[fmt.Sprint(h)] {
			t.Fatalf("request %d: unexpected message %v", h, req)
		}
	}

	// Hanging up must clean everything up
	remote.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConnection didn't return after the peer hung up")
	}
	if p2pPeers.HasAddress("10.0.0.1:2017") {
		t.Error("the connection is still in the peer set")
	}
}

// Regression test: flooding the peers with a new block used to block, holding the peer set lock, on any
// peer whose channel was full, which froze every operation on the peer set.
func TestFloodDoesNotBlockOnStuckPeer(t *testing.T) {
	setupTestSysDbs(t)
	stuck := newTestConnection(nil, "1.2.3.4:5") // Nobody is draining its outbox
	p2pPeers.Add(stuck)
	defer p2pPeers.Remove(stuck)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			p2pCoordinator.floodPeersWithNewBlocks(0, 1)
		}
		p2pPeers.HasAddress("9.9.9.9:1") // The peer set must still be usable
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flooding blocked on a peer which isn't reading")
	}
	if n := len(stuck.outbox); n != 100 {
		t.Errorf("stuck peer has %d queued messages, want 100", n)
	}
}

func TestSlowPeerIsDropped(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	p2pc := newTestConnection(local, "1.2.3.4:5")
	for range p2pMaxOutboxLength {
		p2pc.send("x")
	}
	if n := len(p2pc.outbox); n != p2pMaxOutboxLength {
		t.Fatalf("outbox has %d messages, want %d", n, p2pMaxOutboxLength)
	}
	p2pc.send("one too many")
	remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := remote.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("expected the connection to be closed (EOF), got %v", err)
	}
}

func TestCtrlSendDoesNotBlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range cap(p2pCtrlChannel) + 10 { // More than fits
			p2pCtrlSend(p2pCtrlMessage{msgType: p2pCtrlConnectPeers})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("p2pCtrlSend blocked")
	}
	for len(p2pCtrlChannel) > 0 { // Don't leave junk for other tests
		<-p2pCtrlChannel
	}
}
