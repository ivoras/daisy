package daisy

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

/*
 * Finding the nodes on the local network with multicast DNS (-mdns).
 *
 * The node advertises the service "_daisy._tcp" on the local network, with the hash of the genesis block of its chain
 * in the TXT record, and it looks for the same service of other nodes. The nodes of other chains are ignored.
 * The addresses go to the address book, like all the others, and the node connects to them if it needs peers.
 *
 * It is off by default, because it tells everybody on the network that the computer runs a node.
 */

const (
	mdnsService = "_daisy._tcp"
	mdnsDomain  = "local."

	// How long each search takes, and how long there is between the searches
	mdnsSearchTime   = 10 * time.Second
	mdnsSearchPeriod = time.Minute
)

type mdnsDiscovery struct {
	cancel context.CancelFunc
	done   chan struct{}

	lock   sync.Mutex
	server *zeroconf.Server
}

var mdns *mdnsDiscovery

// startMDNS begins to advertise this node and to look for others, in the background
func startMDNS() *mdnsDiscovery {
	ctx, cancel := context.WithCancel(context.Background())
	m := &mdnsDiscovery{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		m.run(ctx)
	}()
	return m
}

func (m *mdnsDiscovery) stop() {
	if m == nil {
		return
	}
	m.cancel()
	<-m.done
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.server != nil {
		m.server.Shutdown()
		m.server = nil
	}
}

// The name of this node on the local network
func mdnsInstanceName() string {
	return fmt.Sprintf("daisy-%x", p2pEphemeralID)
}

func (m *mdnsDiscovery) run(ctx context.Context) {
	instance := mdnsInstanceName()
	server, err := zeroconf.Register(instance, mdnsService, mdnsDomain, cfg.P2pPort,
		[]string{"root=" + chainParams.GenesisBlockHash, "proto=" + strconv.Itoa(p2pProtocolVersion)}, nil)
	if err != nil {
		log.Println("Cannot advertise the node on the local network:", err)
	} else {
		m.lock.Lock()
		m.server = server
		m.lock.Unlock()
		log.Println("Advertising the node on the local network as", instance)
	}
	for {
		m.search(ctx, instance)
		if !sleepCtx(ctx, mdnsSearchPeriod) {
			return
		}
	}
}

// search looks for the other nodes on the local network for a while
func (m *mdnsDiscovery) search(ctx context.Context, instance string) {
	sctx, cancel := context.WithTimeout(ctx, mdnsSearchTime)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry)
	var found []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case entry := <-entries:
				found = append(found, mdnsPeerAddresses(entry, instance, chainParams.GenesisBlockHash)...)
			case <-sctx.Done():
				return
			}
		}
	}()
	if err := zeroconf.Browse(sctx, mdnsService, mdnsDomain, entries); err != nil && ctx.Err() == nil {
		log.Println("Cannot look for nodes on the local network:", err)
	}
	cancel()
	wg.Wait()
	if len(found) > 0 {
		p2pCtrlSend(p2pCtrlMessage{msgType: p2pCtrlConnectPeers, payload: p2pConnectPeersRequest{
			addresses: found, source: "mdns", allowPrivate: true, trusted: true, keepPorts: true}})
	}
}

// mdnsPeerAddresses returns the addresses of the node that has been found on the local network. It's
// none if the node is us, or is for another blockchain.
func mdnsPeerAddresses(entry *zeroconf.ServiceEntry, ourInstance, root string) []string {
	if entry == nil || entry.Instance == ourInstance || entry.Port < 1 || entry.Port > 65535 {
		return nil
	}
	sameChain := false
	for _, txt := range entry.Text {
		if value, ok := strings.CutPrefix(txt, "root="); ok && value == root {
			sameChain = true
		}
	}
	if !sameChain {
		return nil
	}
	var addresses []string
	for _, ip := range append(append([]net.IP{}, entry.AddrIPv4...), entry.AddrIPv6...) {
		if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() && ip.To4() == nil {
			continue // The link local IPv6 addresses can't be used without knowing the interface
		}
		addresses = append(addresses, net.JoinHostPort(ip.String(), strconv.Itoa(entry.Port)))
	}
	return addresses
}
