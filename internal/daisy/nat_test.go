package daisy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

type fakeRouter struct {
	lock      sync.Mutex
	external  net.IP
	offset    int // The port of the router is ours plus this
	mappings  map[int]int
	adds      int
	deleted   []int
	failAdds  bool
	extAddrOK bool
}

func newFakeRouter(external string) *fakeRouter {
	return &fakeRouter{external: net.ParseIP(external), mappings: map[int]int{}, extAddrOK: true}
}

func (r *fakeRouter) Type() string { return "fake UPnP" }

func (r *fakeRouter) ExternalAddress() (net.IP, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if !r.extAddrOK {
		return nil, errors.New("no external address")
	}
	return r.external, nil
}

func (r *fakeRouter) AddMapping(ctx context.Context, port int, description string, lifetime time.Duration) (int, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.failAdds {
		return 0, errors.New("the router refuses")
	}
	r.adds++
	r.mappings[port] = port + r.offset
	return port + r.offset, nil
}

func (r *fakeRouter) DeleteMapping(ctx context.Context, port int) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	delete(r.mappings, port)
	r.deleted = append(r.deleted, port)
	return nil
}

func (r *fakeRouter) snapshot() (adds int, mappings map[int]int, deleted []int) {
	r.lock.Lock()
	defer r.lock.Unlock()
	m := map[int]int{}
	for k, v := range r.mappings {
		m[k] = v
	}
	return r.adds, m, slices.Clone(r.deleted)
}

func testNATManager(router func() (portMapper, error)) *natManager {
	m := newNATManager(map[int]string{2017: "p2p", 2018: "http"})
	m.discover = func(ctx context.Context) (portMapper, error) { return router() }
	m.lifetime = 100 * time.Millisecond
	m.retry = 20 * time.Millisecond
	m.timeout = time.Second
	selfInfo.setExternal("")
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNATMapsRenewsAndRemovesPorts(t *testing.T) {
	r := newFakeRouter("203.0.113.5")
	m := testNATManager(func() (portMapper, error) { return r, nil })
	m.start()
	waitFor(t, "the ports to be mapped", func() bool { return len(m.getStatus().Ports) == 2 })
	st := m.getStatus()
	if !st.Enabled || st.Type != "fake UPnP" || st.ExternalIP != "203.0.113.5" || st.LastError != "" || st.Ports[2017] != 2017 {
		t.Errorf("unexpected status %+v", st)
	}
	if selfInfo.getExternal() != "203.0.113.5" || !selfInfo.isSelf("203.0.113.5") {
		t.Error("the address of the router should be known to be ours")
	}
	waitFor(t, "the mappings to be renewed", func() bool { adds, _, _ := r.snapshot(); return adds >= 6 })
	m.stop()
	_, mappings, deleted := r.snapshot()
	if len(mappings) != 0 || !slices.Contains(deleted, 2017) || !slices.Contains(deleted, 2018) {
		t.Errorf("the mappings were not removed when the node stopped: %v %v", mappings, deleted)
	}
	if len(m.getStatus().Ports) != 0 {
		t.Error("the ports are still reported as mapped")
	}
}

func TestNATFindsTheRouterEventually(t *testing.T) {
	r := newFakeRouter("203.0.113.6")
	var mu sync.Mutex
	attempts := 0
	m := testNATManager(func() (portMapper, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts <= 2 {
			return nil, errors.New("no router answers")
		}
		return r, nil
	})
	m.start()
	defer m.stop()
	waitFor(t, "the first failure", func() bool {
		return strings.Contains(m.getStatus().LastError, "no router") || len(m.getStatus().Ports) > 0
	})
	waitFor(t, "the ports to be mapped", func() bool { return len(m.getStatus().Ports) == 2 })
	if m.getStatus().LastError != "" {
		t.Errorf("the error should be gone: %s", m.getStatus().LastError)
	}
}

func TestNATWithAnotherNATInFront(t *testing.T) {
	for _, external := range []string{"100.64.3.3", "192.168.0.1"} {
		r := newFakeRouter(external)
		m := testNATManager(func() (portMapper, error) { return r, nil })
		m.start()
		waitFor(t, "the ports to be mapped", func() bool { return len(m.getStatus().Ports) == 2 })
		if st := m.getStatus(); !strings.Contains(st.LastError, "another NAT") {
			t.Errorf("%s: %+v", external, st)
		}
		if selfInfo.getExternal() != "" {
			t.Errorf("%s: the address of a router behind another NAT is not ours", external)
		}
		m.stop()
	}
}

func TestNATPortsMappedDifferently(t *testing.T) {
	r := newFakeRouter("203.0.113.7")
	r.offset = 8
	m := testNATManager(func() (portMapper, error) { return r, nil })
	m.start()
	defer m.stop()
	waitFor(t, "the ports to be mapped", func() bool { return len(m.getStatus().Ports) == 2 })
	if got := m.getStatus().Ports[2017]; got != 2025 {
		t.Errorf("the port of the router is %d", got)
	}
}

func TestNATRouterFailures(t *testing.T) {
	r := newFakeRouter("203.0.113.8")
	r.failAdds = true
	m := testNATManager(func() (portMapper, error) { return r, nil })
	m.start()
	defer m.stop()
	waitFor(t, "the error", func() bool { return strings.Contains(m.getStatus().LastError, "cannot map port") })
	r.lock.Lock()
	r.failAdds = false
	r.lock.Unlock()
	waitFor(t, "the recovery", func() bool { return len(m.getStatus().Ports) == 2 && m.getStatus().LastError == "" })

	r.lock.Lock()
	r.extAddrOK = false
	r.lock.Unlock()
	waitFor(t, "the error about the address", func() bool { return strings.Contains(m.getStatus().LastError, "external address") })
}

func TestNATStopsWhileLookingForTheRouter(t *testing.T) {
	m := newNATManager(map[int]string{2017: "p2p"})
	m.discover = func(ctx context.Context) (portMapper, error) {
		<-ctx.Done() // Nobody answers
		return nil, ctx.Err()
	}
	m.start()
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() { m.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop() didn't return")
	}
	var none *natManager
	none.stop() // Not enabled: fine
	if (&natManager{}).getStatus().Enabled {
		t.Error("a manager that isn't started")
	}
}

func TestCGNAT(t *testing.T) {
	for ip, want := range map[string]bool{"100.64.0.1": true, "100.127.255.255": true, "100.128.0.1": false, "203.0.113.1": false, "10.0.0.1": false} {
		if got := isCGNAT(net.ParseIP(ip)); got != want {
			t.Errorf("isCGNAT(%s) = %v", ip, got)
		}
	}
}

func TestMDNSPeerAddresses(t *testing.T) {
	root := strings.Repeat("ab", 32)
	entry := func(instance string, port int, text []string, v4 []string, v6 []string) *zeroconf.ServiceEntry {
		e := &zeroconf.ServiceEntry{Port: port, Text: text}
		e.Instance = instance
		for _, a := range v4 {
			e.AddrIPv4 = append(e.AddrIPv4, net.ParseIP(a))
		}
		for _, a := range v6 {
			e.AddrIPv6 = append(e.AddrIPv6, net.ParseIP(a))
		}
		return e
	}
	text := []string{"proto=2", "root=" + root}
	for _, tt := range []struct {
		name  string
		entry *zeroconf.ServiceEntry
		want  []string
	}{
		{"a peer", entry("daisy-1", 2017, text, []string{"192.168.1.5"}, nil), []string{"192.168.1.5:2017"}},
		{"both families", entry("daisy-1", 3000, text, []string{"192.168.1.5"}, []string{"fd00::5"}), []string{"192.168.1.5:3000", "[fd00::5]:3000"}},
		{"ourselves", entry("daisy-us", 2017, text, []string{"192.168.1.5"}, nil), nil},
		{"another chain", entry("daisy-2", 2017, []string{"root=" + strings.Repeat("cd", 32)}, []string{"192.168.1.6"}, nil), nil},
		{"no chain", entry("daisy-3", 2017, nil, []string{"192.168.1.7"}, nil), nil},
		{"an invalid port", entry("daisy-4", 0, text, []string{"192.168.1.8"}, nil), nil},
		{"link local IPv6 can't be used", entry("daisy-5", 2017, text, nil, []string{"fe80::1"}), nil},
		{"unusable addresses", entry("daisy-6", 2017, text, []string{"0.0.0.0", "224.0.0.1"}, nil), nil},
		{"nothing", nil, nil},
	} {
		if got := mdnsPeerAddresses(tt.entry, "daisy-us", root); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Needs a network where multicast works, so it's only run when asked to: a second "node" is advertised, and
// the search must find it.
func TestMDNSOnTheLocalNetwork(t *testing.T) {
	if os.Getenv("DAISY_TEST_MDNS") == "" {
		t.Skip("set DAISY_TEST_MDNS=1 to try multicast DNS on this machine's network")
	}
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	cfg.P2pPort = DefaultP2PPort

	other, err := zeroconf.Register("daisy-other-node", mdnsService, mdnsDomain, 20999,
		[]string{"root=" + tc.genesis.hash, "proto=2"}, nil)
	if err != nil {
		t.Skip("cannot advertise on this network:", err)
	}
	defer other.Shutdown()
	stranger, err := zeroconf.Register("daisy-another-chain", mdnsService, mdnsDomain, 20998,
		[]string{"root=" + strings.Repeat("ee", 32)}, nil)
	if err != nil {
		t.Skip("cannot advertise on this network:", err)
	}
	defer stranger.Shutdown()

	drainCtrl()
	m := &mdnsDiscovery{}
	m.search(context.Background(), mdnsInstanceName())
	var found []string
	for _, msg := range drainCtrl() {
		if req, ok := msg.payload.(p2pConnectPeersRequest); ok && req.source == "mdns" {
			found = append(found, req.addresses...)
		}
	}
	has := func(suffix string) bool {
		for _, a := range found {
			if strings.HasSuffix(a, suffix) {
				return true
			}
		}
		return false
	}
	if !has(":20999") {
		t.Errorf("the other node was not found: %v", found)
	}
	if has(":20998") {
		t.Errorf("a node of another blockchain was returned: %v", found)
	}
}

func TestStatusAndPeersEndpoints(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "1")
	tc.mustAccept(b1, blockNewTip)
	abGood("203.0.113.30:2017")
	abAdd("203.0.113.31:2017", "x") // Only heard of: not given out
	selfInfo.setExternal("")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /peers", blockWebSendPeers)
	mux.HandleFunc("GET /status", blockWebSendStatus)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	list, err := fetchPeerList(srv.URL + "/peers")
	if err != nil || !slices.Contains(list, "203.0.113.30:2017") || slices.Contains(list, "203.0.113.31:2017") {
		t.Errorf("peers: %v %v", list, err)
	}
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st nodeStatus
	if err = json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Protocol != p2pProtocolVersion || st.MinProtocol != p2pMinProtocolVersion || st.Height != 1 || st.TipHash != b1.hash ||
		st.ChainScore != 2 || st.Root != tc.genesis.hash || st.Version != p2pClientVersionString {
		t.Errorf("unexpected status %+v", st)
	}
	if st.AddressBook.Tried < 1 || st.AddressBook.New != 1 || st.NAT.Enabled || st.Reachable {
		t.Errorf("unexpected status %+v", st)
	}
}

func TestPullPeers(t *testing.T) {
	setupTestSysDbs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/peers" {
			w.Write([]byte(`{"peers": ["203.0.113.40:2017", "203.0.114.40:2017"]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	pullPeers(srv.URL + "/")
	for _, a := range []string{"127.0.0.1:2017", "203.0.113.40:2017", "203.0.114.40:2017"} {
		if !bookHas(a) {
			t.Errorf("%s is not in the address book", a)
		}
	}
	// A server without the list, or an invalid address, is no problem
	pullPeers("http://127.0.0.1:1/")
	pullPeers("nonsense")
}
