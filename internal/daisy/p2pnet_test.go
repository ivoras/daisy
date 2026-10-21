package daisy

import (
	"bufio"
	"errors"
	"fmt"
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

// Duplicate and self connections: only the outbound ones are remembered as bad dial addresses, because the
// address of an inbound connection contains a random source port.
func TestDuplicateConnectionBans(t *testing.T) {
	setupTestSysDbs(t)
	existing := newTestConnection(nil, "198.51.100.1:2017")
	existing.peerID = 4242
	p2pPeers.Add(existing)
	defer p2pPeers.Remove(existing)

	for _, outbound := range []bool{true, false} {
		local, remote := net.Pipe()
		defer remote.Close()
		address := fmt.Sprintf("198.51.100.2:%d", map[bool]int{true: 2017, false: 40000}[outbound])
		p2pc := newTestConnection(local, address)
		p2pc.outbound = outbound
		hello := helloMsg(4242, 0, "", 0)
		p2pc.handleMsgHello(hello)
		if got := p2pCoordinator.badPeers.Has(address); got != outbound {
			t.Errorf("outbound=%v: badPeers.Has(%s) = %v", outbound, address, got)
		}
	}
}
