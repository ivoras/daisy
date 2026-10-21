package daisy

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

func TestReadLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("short\n"+strings.Repeat("x", 10000)+"\nlast"), 16)
	if line, err := readLine(r, 100); err != nil || string(line) != "short\n" {
		t.Fatalf("got %q, %v", line, err)
	}
	if _, err := readLine(r, 100); !errors.Is(err, errLineTooLong) {
		t.Fatalf("a long line should be refused, got %v", err)
	}

	// A line spanning many buffer fills is fine within the limit, and exactly at the limit
	long := strings.Repeat("y", 999) + "\n"
	r = bufio.NewReaderSize(strings.NewReader(long+long), 16)
	for range 2 {
		if line, err := readLine(r, 1000); err != nil || string(line) != long {
			t.Fatalf("got %d bytes, %v", len(line), err)
		}
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader(long)), 999); !errors.Is(err, errLineTooLong) {
		t.Fatalf("one byte over the limit should be refused, got %v", err)
	}
	if _, err := readLine(r, 1000); err == nil {
		t.Fatal("expected EOF")
	}
}

func TestCheckBlockSize(t *testing.T) {
	for size, ok := range map[int64]bool{
		-1: false, 0: false, 1: true, 1 << 20: true, p2pMaxBlockSize: true, p2pMaxBlockSize + 1: false,
	} {
		if err := checkBlockSize(size); (err == nil) != ok {
			t.Errorf("checkBlockSize(%d) = %v", size, err)
		}
	}
}

func TestBannedPeers(t *testing.T) {
	co := &p2pCoordinatorType{
		badPeers: util.NewStringSetWithExpiry(time.Minute),
		badHosts: util.NewStringSetWithExpiry(time.Minute),
	}
	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 2017}
	if co.isBanned(addr) {
		t.Fatal("not banned yet")
	}
	co.badPeers.Add("203.0.113.9:2017")
	if !co.isBanned(addr) {
		t.Error("a banned dial address isn't recognised")
	}
	if co.isBanned(&net.TCPAddr{IP: addr.IP, Port: 2018}) {
		t.Error("a ban of a dial address shouldn't affect other ports")
	}
	co.badHosts.Add("203.0.113.10")
	for _, port := range []int{1, 2017, 50000} {
		if !co.isBanned(&net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: port}) {
			t.Errorf("a banned host should be banned on port %d", port)
		}
	}
}

// pipeConnection is a connection of the given kind with a fake remote address, registered with the peers
func pipeConnection(t *testing.T, address string, outbound bool) *p2pConnection {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { local.Close(); remote.Close() })
	p2pc := newTestConnection(&fakeAddrConn{Conn: local, remote: address}, address)
	p2pc.outbound = outbound
	p2pPeers.Add(p2pc)
	t.Cleanup(func() { p2pPeers.Remove(p2pc) })
	return p2pc
}

// connectionClosed tells if the other end of the connection sees it closed
func connectionClosed(c *p2pConnection) bool {
	c.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := c.conn.Write([]byte("x"))
	return errors.Is(err, io.ErrClosedPipe)
}

// An attacker who claims the ID of an honest peer H (all our messages used to tell it) mustn't get H banned or disconnected.
func TestClaimingAnotherPeersIDBansNobody(t *testing.T) {
	setupTestSysDbs(t)
	drainCtrl()
	const honestID = 777001

	// We are connected to H, and H is also the address we dial
	h := pipeConnection(t, "198.51.100.1:2017", true)
	h.handleMsgHello(helloMsg(honestID, 0, "", 0))
	if !h.handshaken {
		t.Fatal("H's hello was not accepted")
	}

	// The attacker, from another IP address, claims H's ID, inbound and outbound
	for _, outbound := range []bool{false, true} {
		address := map[bool]string{false: "203.0.113.66:40000", true: "203.0.113.67:2017"}[outbound]
		attacker := pipeConnection(t, address, outbound)
		attacker.handleMsgHello(helloMsg(honestID, 0, "", 0))
		if !attacker.handshaken {
			t.Errorf("outbound=%v: the attacker's connection was dropped, it is nobody's duplicate", outbound)
		}
		for _, bad := range []string{"198.51.100.1:2017", "198.51.100.1", address} {
			if p2pCoordinator.badPeers.Has(bad) || p2pCoordinator.badHosts.Has(bad) {
				t.Errorf("outbound=%v: %s was banned", outbound, bad)
			}
		}
	}
	if connectionClosed(h) || !h.handshaken {
		t.Error("H's connection was affected")
	}

	// The other way round: H comes after the attacker. It isn't banned either.
	h2 := pipeConnection(t, "198.51.100.2:2017", true)
	h2.handleMsgHello(helloMsg(honestID, 0, "", 0))
	if !h2.handshaken || p2pCoordinator.badPeers.Has("198.51.100.2:2017") {
		t.Error("H2 was dropped or banned for a claimed ID")
	}
}

// Two connections of one node (the same IP address and ID): the newer one is dropped, and nothing is banned.
func TestSecondConnectionOfANodeIsDropped(t *testing.T) {
	setupTestSysDbs(t)
	drainCtrl()
	first := pipeConnection(t, "198.51.100.1:40000", false)
	first.handleMsgHello(helloMsg(777002, 0, "", 0))
	if !first.handshaken {
		t.Fatal("the first connection was not accepted")
	}
	for _, outbound := range []bool{true, false} {
		address := map[bool]string{true: "198.51.100.1:2017", false: "198.51.100.1:40001"}[outbound]
		second := pipeConnection(t, address, outbound)
		second.handleMsgHello(helloMsg(777002, 0, "", 0))
		if second.handshaken || !connectionClosed(second) {
			t.Errorf("outbound=%v: the second connection of the node was kept", outbound)
		}
		if p2pCoordinator.badPeers.Has(address) {
			t.Errorf("outbound=%v: the second connection got the address banned", outbound)
		}
	}
	if connectionClosed(first) || !first.handshaken {
		t.Error("the first connection was dropped")
	}
}

// A connection which claims our ID is dropped and only its own address is avoided. An attacker who learned our ID can't
// do more than that.
func TestRealSelfConnectionIsDetected(t *testing.T) {
	setupTestSysDbs(t)
	drainCtrl()
	abAdd("203.0.113.44:2017", "a")
	abAdd("198.51.100.1:2017", "a")
	h := pipeConnection(t, "198.51.100.1:2017", true)
	h.handleMsgHello(helloMsg(777003, 0, "", 0))

	// An inbound one (we have dialed ourselves, this is the other end) is just dropped
	in := pipeConnection(t, "203.0.113.44:40000", false)
	in.handleMsgHello(helloMsg(int(p2pEphemeralID), 0, "", 0))
	if in.handshaken || !connectionClosed(in) {
		t.Error("an inbound connection with our ID was kept")
	}
	if selfInfo.isSelf("203.0.113.44") {
		t.Error("an inbound claim makes an address ours")
	}
	if p2pCoordinator.badPeers.Has("203.0.113.44:40000") {
		t.Error("the random source port of an inbound connection was banned")
	}

	// An outbound one is the address we have dialed: it's ours (or its owner says so)
	out := pipeConnection(t, "203.0.113.44:2017", true)
	out.savedAddress = "203.0.113.44:2017"
	out.handleMsgHello(helloMsg(int(p2pEphemeralID), 0, "", 0))
	if out.handshaken || !connectionClosed(out) {
		t.Error("an outbound connection to ourselves was kept")
	}
	defer selfInfo.lock.With(func() { selfInfo.selfHosts = make(map[string]time.Time) })
	if !selfInfo.isSelf("203.0.113.44") || !p2pCoordinator.badPeers.Has("203.0.113.44:2017") || bookHas("203.0.113.44:2017") {
		t.Error("the address of the connection to ourselves is not avoided")
	}

	// The honest peer is not touched by any of this
	if selfInfo.isSelf("198.51.100.1") || p2pCoordinator.badPeers.Has("198.51.100.1:2017") || !bookHas("198.51.100.1:2017") {
		t.Error("H's address was affected")
	}
	if connectionClosed(h) || !h.handshaken {
		t.Error("H's connection was dropped")
	}
}

// Messages other than hello don't carry the ID
func TestOnlyHelloCarriesTheID(t *testing.T) {
	ping, err := json.Marshal(p2pMsgPingStruct{p2pMsgHeader{Root: "r", Msg: p2pMsgPing}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ping), "p2p_id") {
		t.Errorf("a ping has the ID: %s", ping)
	}
	hello, _ := json.Marshal(p2pMsgHelloStruct{p2pMsgHeader: p2pMsgHeader{P2pID: 5, Msg: p2pMsgHello}})
	if !strings.Contains(string(hello), `"p2p_id":5`) {
		t.Errorf("the hello has no ID: %s", hello)
	}
}
