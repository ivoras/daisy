package daisy

import (
	"bufio"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

func bookCounts(t *testing.T) (newAddresses, tried int) {
	t.Helper()
	return abCounts()
}

func bookHas(address string) bool {
	return abExistsLocked(address)
}

func abExistsLocked(address string) bool {
	abLock.Lock()
	defer abLock.Unlock()
	return abExists(address)
}

func TestNetworkGroups(t *testing.T) {
	for host, want := range map[string]string{
		"203.0.113.77":        "v4:203.0.113",
		"203.0.200.1":         "v4:203.0",
		"2001:db8:1234::1":    "v6:2001:0db8",
		"::ffff:198.51.100.9": "v4:198.51",
		"Seed.Example.COM":    "dns:seed.example.com",
		"192.168.1.5":         "",
		"10.0.0.1":            "",
		"127.0.0.1":           "",
		"::1":                 "",
		"169.254.1.1":         "",
		"fe80::1":             "",
		"printer.local":       "",
		"localhost":           "",
	} {
		got := networkGroup(host)
		if host == "203.0.113.77" {
			want = "v4:203.0" // The group is the /16
		}
		if got != want {
			t.Errorf("networkGroup(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestAddressBookRejectsInvalidAddresses(t *testing.T) {
	setupTestSysDbs(t)
	for _, a := range []string{"", "host", "host:0", "host:70000", "host:abc", ":2017", "0.0.0.0:2017", "224.0.0.1:2017", "[::]:2017",
		strings.Repeat("a", 300) + ":2017"} {
		if abAdd(a, "test") {
			t.Errorf("%q was accepted", a)
		}
	}
	if !abAdd("203.0.113.1:2017", "test") || !abAdd("[2001:db8::1]:2017", "test") || !abAdd("peers.example.com:2017", "test") {
		t.Error("valid addresses were refused")
	}
	if abAdd("203.0.113.1:2017", "test") {
		t.Error("an address which is already in the book was added again")
	}
}

func TestAddressBookLimitsPerGroupAndSource(t *testing.T) {
	setupTestSysDbs(t)
	// One network (a /16) can't take over the new addresses
	for i := 0; i < abMaxNewPerGroup+5; i++ {
		ok := abAdd(fmt.Sprintf("198.51.100.%d:2017", i+1), fmt.Sprintf("source%d", i))
		if ok != (i < abMaxNewPerGroup) {
			t.Fatalf("address %d: accepted=%v", i, ok)
		}
	}
	if !abAdd("203.0.113.1:2017", "other") {
		t.Error("another network should be fine")
	}
	// ... but private networks aren't limited, a company network has all its nodes in one /16
	for i := 0; i < 30; i++ {
		if !abAdd(fmt.Sprintf("192.168.1.%d:2017", i+1), fmt.Sprintf("lan%d", i)) {
			t.Fatalf("private address %d was refused", i)
		}
	}
	// One source can't tell us about more than a limited number of addresses either (they are in different networks here)
	for i := 0; i < abMaxNewPerSource+20; i++ {
		ok := abAdd(fmt.Sprintf("%d.%d.0.1:2017", 20+i/200, i%200), "203.0.0.99")
		if ok != (i < abMaxNewPerSource) {
			t.Fatalf("address %d from the same source: accepted=%v", i, ok)
		}
	}
	// The sources in the same network count as one
	if abAdd("45.1.0.1:2017", "203.0.77.77") {
		t.Error("a source in the same /16 as one which has used up its quota")
	}
	if !abAdd("45.1.0.1:2017", "100.64.0.1") {
		t.Error("a source in another network has its own quota")
	}
}

func TestAddressBookSizeLimit(t *testing.T) {
	setupTestSysDbs(t)
	old := abMaxNew
	abMaxNew = 10
	defer func() { abMaxNew = old }()
	for i := 0; i < 10; i++ {
		abAdd(fmt.Sprintf("%d.1.0.1:2017", 30+i), fmt.Sprintf("s%d", i))
	}
	abAddBootstrap("seed.example.com:2017") // Never evicted
	abFailed("30.1.0.1:2017")
	abFailed("30.1.0.1:2017")
	if !abAdd("50.1.0.1:2017", "s50") {
		t.Fatal("a full book should make room")
	}
	if bookHas("30.1.0.1:2017") {
		t.Error("the address which failed should have been the one to go")
	}
	if n, _ := bookCounts(t); n > abMaxNew {
		t.Errorf("%d new addresses, the limit is %d", n, abMaxNew)
	}
	if !bookHas("seed.example.com:2017") {
		t.Error("the bootstrap peer was removed")
	}
}

func TestAddressBookTiers(t *testing.T) {
	setupTestSysDbs(t)
	if !abAdd("203.0.113.5:2017", "source") {
		t.Fatal()
	}
	if n, tried := bookCounts(t); n != 1 || tried != 0 {
		t.Fatalf("new=%d tried=%d", n, tried)
	}
	abFailed("203.0.113.5:2017")
	if !abGood("203.0.113.5:2017") {
		t.Error("the first success should be reported")
	}
	if abGood("203.0.113.5:2017") {
		t.Error("the second success is not news")
	}
	if n, tried := bookCounts(t); n != 0 || tried != 1 {
		t.Fatalf("new=%d tried=%d", n, tried)
	}
	var failures int
	mainDb.QueryRow("SELECT failures FROM peers WHERE address='203.0.113.5:2017'").Scan(&failures)
	if failures != 0 {
		t.Error("a success should clear the failures")
	}
	// A peer that wasn't in the book (it has connected to us) goes straight in among the tried ones
	abGood("198.18.0.1:2017")
	if _, tried := bookCounts(t); tried != 2 {
		t.Error("an unknown peer which said hello is a tried address")
	}
	// Attempts to promote an invalid address do nothing
	if abGood("nonsense") {
		t.Error("nonsense was promoted")
	}

	// Too many tried addresses in one network: the oldest goes back among the new ones
	oldGroup := abMaxTriedPerGroup
	abMaxTriedPerGroup = 2
	defer func() { abMaxTriedPerGroup = oldGroup }()
	for i := 1; i <= 3; i++ {
		abGood(fmt.Sprintf("100.64.0.%d:2017", i))
		mainDb.Exec("UPDATE peers SET last_success=? WHERE address=?", 1000+i, fmt.Sprintf("100.64.0.%d:2017", i))
	}
	abGood("100.64.0.4:2017")
	var tried int
	mainDb.QueryRow("SELECT COUNT(*) FROM peers WHERE tried=1 AND net_group='v4:100.64'").Scan(&tried)
	if tried != 2 {
		t.Errorf("%d tried addresses in the network, the limit is 2", tried)
	}
	var isTried bool
	mainDb.QueryRow("SELECT tried FROM peers WHERE address='100.64.0.1:2017'").Scan(&isTried)
	if isTried {
		t.Error("the oldest should have been demoted")
	}
	if !bookHas("100.64.0.1:2017") {
		t.Error("... not removed")
	}
	// The same for the total
	oldTotal := abMaxTried
	abMaxTried = 3
	defer func() { abMaxTried = oldTotal }()
	abGood("151.101.0.1:2017")
	abPrune()
	if _, tried := bookCounts(t); tried > 3 {
		t.Errorf("%d tried addresses, the limit is 3", tried)
	}
}

func TestAddressBookBootstrapPeers(t *testing.T) {
	setupTestSysDbs(t)
	abAddBootstrap("cosmos.example.com:2017")
	abAddBootstrap("cosmos.example.com:2017") // Again is fine
	abAddBootstrap("invalid")
	for range peerMaxFailures + 3 {
		abFailed("cosmos.example.com:2017")
	}
	mainDb.Exec("UPDATE peers SET time_added=1, last_try=1")
	abPrune()
	if !bookHas("cosmos.example.com:2017") {
		t.Fatal("a bootstrap peer must never be forgotten")
	}
	var tried, permanent bool
	mainDb.QueryRow("SELECT tried, permanent FROM peers").Scan(&tried, &permanent)
	if !tried || !permanent {
		t.Errorf("tried=%v permanent=%v", tried, permanent)
	}
	// ... and it's tried even if it has been failing, once the time since the last attempt is long enough
	if got := abSelect(5, nil, nil); !slices.Equal(got, []string{"cosmos.example.com:2017"}) {
		t.Errorf("selected %v", got)
	}
	if got := abList(10); len(got) != 0 {
		t.Errorf("a bootstrap peer that is failing should not be given out: %v", got)
	}
}

func TestAddressBookPrune(t *testing.T) {
	setupTestSysDbs(t)
	abAdd("203.0.113.1:2017", "a")
	abAdd("203.0.114.1:2017", "b")
	abAdd("203.0.115.1:2017", "c")
	for range peerMaxFailures {
		abFailed("203.0.113.1:2017")
	}
	mainDb.Exec("UPDATE peers SET time_added=? WHERE address='203.0.114.1:2017'", time.Now().Add(-2*peerMaxAge).Unix())
	abPrune()
	if bookHas("203.0.113.1:2017") || bookHas("203.0.114.1:2017") {
		t.Error("dead and old addresses should be forgotten")
	}
	if !bookHas("203.0.115.1:2017") {
		t.Error("a good address was forgotten")
	}
}

func TestAddressBookSelection(t *testing.T) {
	setupTestSysDbs(t)
	old := abRandFloat
	defer func() { abRandFloat = old }()

	// 3 networks, 4 addresses each, half of them tried
	for n := 1; n <= 3; n++ {
		for i := 1; i <= 4; i++ {
			a := fmt.Sprintf("%d.%d.0.%d:2017", 60+n, n, i)
			abAdd(a, fmt.Sprintf("s%d%d", n, i))
			if i <= 2 {
				abGood(a)
			}
		}
	}
	mainDb.Exec("UPDATE peers SET last_try=0") // abGood has just "tried" them
	got := abSelect(10, nil, nil)
	if len(got) != 3 {
		t.Fatalf("selected %d addresses, but there are only 3 networks: %v", len(got), got)
	}
	groups := map[string]bool{}
	for _, a := range got {
		host, _ := splitPeerAddress(a)
		g := networkGroup(host)
		if groups[g] {
			t.Errorf("two addresses of the network %s", g)
		}
		groups[g] = true
	}
	// Networks in use are avoided
	got = abSelect(10, map[string]bool{"v4:61.1": true, "v4:62.2": true}, nil)
	if len(got) != 1 || !strings.HasPrefix(got[0], "63.") {
		t.Errorf("selected %v", got)
	}
	// The skip function
	got = abSelect(10, nil, func(a string) bool { return strings.HasPrefix(a, "61.") })
	if len(got) != 2 {
		t.Errorf("selected %v", got)
	}
	// Tried or new first, depending on the dice
	isTried := func(a string) bool {
		var tried bool
		mainDb.QueryRow("SELECT tried FROM peers WHERE address=?", a).Scan(&tried)
		return tried
	}
	abRandFloat = func() float64 { return 0.0 }
	for range 10 {
		if got = abSelect(1, nil, nil); len(got) != 1 || !isTried(got[0]) {
			t.Fatalf("expected a tried address, got %v", got)
		}
	}
	abRandFloat = func() float64 { return 0.99 }
	for range 10 {
		if got = abSelect(1, nil, nil); len(got) != 1 || isTried(got[0]) {
			t.Fatalf("expected a new address, got %v", got)
		}
	}
	// A failing address waits longer and longer, and is given up on
	abRandFloat = old
	for _, a := range abSelect(100, nil, nil) {
		mainDb.Exec("UPDATE peers SET last_try=? WHERE address=?", 0, a)
	}
	abAdd("70.9.0.1:2017", "x")
	abFailed("70.9.0.1:2017")
	if slices.Contains(abSelect(100, nil, nil), "70.9.0.1:2017") {
		t.Error("an address which has just failed was selected")
	}
	mainDb.Exec("UPDATE peers SET last_try=? WHERE address='70.9.0.1:2017'", time.Now().Add(-abBackoff(1)-time.Second).Unix())
	if !slices.Contains(abSelect(100, nil, nil), "70.9.0.1:2017") {
		t.Error("the backoff should have passed")
	}
	if abBackoff(1) <= abBackoff(0) || abBackoff(100) > time.Hour {
		t.Errorf("backoff: %v %v %v", abBackoff(0), abBackoff(1), abBackoff(100))
	}
	for range peerMaxFailures {
		abFailed("70.9.0.1:2017")
	}
	mainDb.Exec("UPDATE peers SET last_try=0")
	if slices.Contains(abSelect(100, nil, nil), "70.9.0.1:2017") {
		t.Error("an address which has failed too many times was selected")
	}
}

func TestAddressBookMigration(t *testing.T) {
	// The tables as the previous versions made them
	dir := t.TempDir()
	var err error
	if mainDb, err = sql.Open("sqlite3", filepath.Join(dir, "old.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mainDb.Close(); mainDb = nil })
	for _, q := range []string{
		`CREATE TABLE blockchain (height INTEGER NOT NULL UNIQUE, sigkey_hash VARCHAR NOT NULL, hash VARCHAR NOT NULL PRIMARY KEY, hash_signature VARCHAR NOT NULL,
			prev_hash VARCHAR NOT NULL, prev_hash_signature VARCHAR NOT NULL, time_accepted INTEGER NOT NULL, version INTEGER NOT NULL)`,
		`CREATE TABLE pubkeys (pubkey_hash VARCHAR NOT NULL PRIMARY KEY, pubkey VARCHAR NOT NULL, state CHAR NOT NULL, time_added INTEGER NOT NULL,
			time_revoked INTEGER, block_height INTEGER NOT NULL, metadata VARCHAR)`,
		`CREATE TABLE config (key VARCHAR NOT NULL PRIMARY KEY, value VARCHAR NOT NULL)`,
		`CREATE TABLE peers (address VARCHAR NOT NULL PRIMARY KEY, time_added INTEGER NOT NULL, permanent BOOLEAN NOT NULL DEFAULT 0)`,
		`INSERT INTO peers VALUES ('cosmos.example.com:2017', 1000, 1)`,
		`INSERT INTO peers VALUES ('203.0.113.9:2017', 2000, 0)`,
		`INSERT INTO peers VALUES ('192.168.1.2:2017', 3000, 0)`,
	} {
		if _, err = mainDb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	dbMigrate(false)
	dbMigrate(false) // Twice is fine
	type row struct {
		tried     bool
		permanent bool
		group     string
	}
	got := map[string]row{}
	rows, err := mainDb.Query("SELECT address, tried, permanent, net_group FROM peers")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		var r row
		rows.Scan(&a, &r.tried, &r.permanent, &r.group)
		got[a] = r
	}
	rows.Close()
	want := map[string]row{
		"cosmos.example.com:2017": {true, true, "dns:cosmos.example.com"},
		"203.0.113.9:2017":        {false, false, "v4:203.0"},
		"192.168.1.2:2017":        {false, false, ""},
	}
	for a, w := range want {
		if got[a] != w {
			t.Errorf("%s: %+v, want %+v", a, got[a], w)
		}
	}
	if abCount("1=1") != 3 {
		t.Error("the peers were lost")
	}
}

func TestParsePeerList(t *testing.T) {
	want := []string{"203.0.113.1:2017", "203.0.113.2:3000", "[2001:db8::1]:2017", "peer.example.com:2017"}
	for name, body := range map[string]string{
		"object": `{"peers": ["203.0.113.1", "203.0.113.2:3000", "[2001:db8::1]:2017", "peer.example.com", "", "bad:port:here:x"]}`,
		"array":  `["203.0.113.1:2017", "203.0.113.2:3000", "2001:db8::1", "peer.example.com:2017"]`,
		"lines":  "# the peers\n203.0.113.1\n  203.0.113.2:3000  # a comment\n\n[2001:db8::1]:2017\npeer.example.com:2017\n",
	} {
		got := parsePeerList([]byte(body))
		// The invalid entries are dropped; compare ignoring the few that are not in every form
		if name == "array" {
			want2 := []string{"203.0.113.1:2017", "203.0.113.2:3000", "[2001:db8::1]:2017", "peer.example.com:2017"}
			if !slices.Equal(got, want2) {
				t.Errorf("%s: %v", name, got)
			}
			continue
		}
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("%s: %v doesn't have %s", name, got, w)
			}
		}
	}
}

func TestFetchPeerList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/peers":
			w.Write([]byte(`{"peers": ["203.0.113.1:2017"]}`))
		case "/huge":
			w.Write([]byte(strings.Repeat("203.0.113.1\n", maxPeerListSize/10)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if got, err := fetchPeerList(srv.URL + "/peers"); err != nil || len(got) != 1 {
		t.Errorf("%v %v", got, err)
	}
	for _, path := range []string{"/missing", "/huge"} {
		if _, err := fetchPeerList(srv.URL + path); err == nil {
			t.Errorf("%s was accepted", path)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://example.com/peers", "nonsense"} {
		if _, err := fetchPeerList(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestResolveSeed(t *testing.T) {
	got := resolveSeed("localhost:2999")
	if len(got) == 0 {
		t.Skip("localhost doesn't resolve")
	}
	for _, a := range got {
		if !strings.HasSuffix(a, ":2999") {
			t.Errorf("unexpected address %s", a)
		}
	}
	if got := resolveSeed("localhost"); len(got) == 0 || !strings.HasSuffix(got[0], fmt.Sprintf(":%d", DefaultP2PPort)) {
		t.Errorf("the default port is used if there is none: %v", got)
	}
	if got := resolveSeed("no-such-host.invalid"); len(got) != 0 {
		t.Errorf("resolved %v", got)
	}
	if got := resolveSeed(""); len(got) != 0 {
		t.Errorf("resolved %v", got)
	}
}

// With enough outbound connections, nobody is dialed
func fillOutbound(t *testing.T, n int) {
	t.Helper()
	var conns []*p2pConnection
	for i := range n {
		c := &p2pConnection{address: fmt.Sprintf("198.18.%d.1:2017", i), outbound: true}
		p2pPeers.Add(c)
		conns = append(conns, c)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			p2pPeers.Remove(c)
		}
	})
}

func TestGossipedAddressesGoToTheAddressBook(t *testing.T) {
	setupTestSysDbs(t)
	fillOutbound(t, p2pTargetOutbound) // So that nothing is dialed
	co := &p2pCoordinatorType{
		badPeers: util.NewStringSetWithExpiry(time.Minute),
		badHosts: util.NewStringSetWithExpiry(time.Minute),
	}
	var gossip []string
	for i := 0; i < 40; i++ {
		gossip = append(gossip, fmt.Sprintf("%d.%d.0.1:%d", 80+i, i, 3000+i)) // The ports are not the default ones
	}
	co.handleConnectPeers(p2pConnectPeersRequest{addresses: gossip, source: "203.0.113.200"})
	if n, _ := bookCounts(t); n != p2pMaxPeerAddresses {
		t.Errorf("%d addresses were taken from a peer, the limit is %d", n, p2pMaxPeerAddresses)
	}
	if !bookHas("80.0.0.1:2017") || bookHas("80.0.0.1:3000") {
		t.Error("gossiped addresses are always on the default port")
	}

	// Addresses of private networks are of no use unless the peer is on a private network
	co.handleConnectPeers(p2pConnectPeersRequest{addresses: []string{"192.168.5.5:2017", "127.0.0.1:2017", "localhost:2017"}, source: "203.0.113.201"})
	if bookHas("192.168.5.5:2017") || bookHas("127.0.0.1:2017") {
		t.Error("a peer on the Internet made us remember private addresses")
	}
	co.handleConnectPeers(p2pConnectPeersRequest{addresses: []string{"192.168.5.5:2017"}, source: "192.168.5.9", allowPrivate: true})
	if !bookHas("192.168.5.5:2017") {
		t.Error("a peer on a private network can tell about its neighbours")
	}
	// A trusted source (a seed, a list) can give many addresses with their ports
	var seeds []string
	for i := 0; i < 100; i++ {
		seeds = append(seeds, fmt.Sprintf("%d.1.0.1:4000", 120+i))
	}
	co.handleConnectPeers(p2pConnectPeersRequest{addresses: seeds, source: "seed", trusted: true, keepPorts: true})
	if !bookHas("120.1.0.1:4000") {
		t.Error("the port of a seed's address must be kept")
	}
	if n := abCount("source_group='seed'"); n != abMaxNewPerSource {
		t.Errorf("%d addresses from the seeds", n)
	}
}

func TestMaintainConnectionsDials(t *testing.T) {
	setupTestSysDbs(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	address := l.Addr().String()
	abAddBootstrap(address)
	co := &p2pCoordinatorType{
		badPeers: util.NewStringSetWithExpiry(time.Minute),
		badHosts: util.NewStringSetWithExpiry(time.Minute),
	}
	co.maintainConnections()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("nobody connected to the address in the book")
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	hello := readJSONLine(t, bufio.NewReader(conn))
	if hello["msg"] != p2pMsgHello || hello["protocol"] != float64(p2pProtocolVersion) {
		t.Errorf("unexpected hello %v", hello)
	}
	if hello["observed_host"] != "127.0.0.1" {
		t.Errorf("observed_host: %v", hello["observed_host"])
	}
	deadline := time.Now().Add(2 * time.Second)
	for co.pendingDials.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if co.pendingDials.Load() != 0 {
		t.Error("the connection is still counted as being made")
	}
	if p2pPeers.Count(true) != 1 {
		t.Errorf("%d outbound connections", p2pPeers.Count(true))
	}
	// It was tried now, and it's connected: nothing more to do
	co.maintainConnections()
	time.Sleep(100 * time.Millisecond)
	conn.Close() // The connection handler ends and removes itself from the peers
	deadline = time.Now().Add(2 * time.Second)
	for p2pPeers.Count(true) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p2pPeers.Count(true) != 0 {
		t.Error("the connection was not cleaned up")
	}
}

func TestConnectionToMyselfIsForgotten(t *testing.T) {
	setupTestSysDbs(t)
	abAdd("203.0.113.44:2017", "a")
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	p2pc := newTestConnection(&fakeAddrConn{Conn: local, remote: "203.0.113.44:2017"}, "203.0.113.44:2017")
	p2pc.outbound = true
	p2pc.savedAddress = "203.0.113.44:2017"
	p2pc.handleMsgHello(helloMsg(int(p2pEphemeralID), 0, "", 0))
	if bookHas("203.0.113.44:2017") {
		t.Error("our own address is still in the book")
	}
	if !selfInfo.isSelf("203.0.113.44") {
		t.Error("the address should be known to be ours")
	}
	selfInfo.lock.With(func() { selfInfo.selfHosts = make(map[string]time.Time) })
	if p2pPeers.HasAddress("203.0.113.44:2017") {
		t.Error("left in the peers")
	}
}
