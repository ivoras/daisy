package daisy

import (
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

// Messages to the p2p controller goroutine
const (
	p2pCtrlSearchForBlocks = iota
	p2pCtrlHaveNewBlock
	p2pCtrlConnectPeers
)

type p2pCtrlMessage struct {
	msgType int
	payload any
}

var p2pCtrlChannel = make(chan p2pCtrlMessage, 64)

// Data related to the (single instance of) the global p2p coordinator. This is also a
// single-threaded object, its fields and methods are only expected to be accessed from
// the Run() goroutine.
type p2pCoordinatorType struct {
	timeTicks               chan int
	lastTickTipHash         string
	lastTickTipHeight       int
	pendingDials            atomic.Int32 // Connections being made at the moment
	recentlyRequestedBlocks *util.StringSetWithExpiry
	lastReconnectTime       time.Time
	badPeers                *util.StringSetWithExpiry // Addresses ("ip:port") we have dialed and shouldn't dial again
	badHosts                *util.StringSetWithExpiry // Hosts (IP addresses) whose connections are refused and not dialed
}

// XXX: singletons in go?
var p2pCoordinator = p2pCoordinatorType{
	recentlyRequestedBlocks: util.NewStringSetWithExpiry(5 * time.Second),
	lastReconnectTime:       time.Now(),
	timeTicks:               make(chan int),
	badPeers:                util.NewStringSetWithExpiry(15 * time.Minute),
	badHosts:                util.NewStringSetWithExpiry(15 * time.Minute),
}

// The payload of p2pCtrlSearchForBlocks: look for better blocks than ours at this peer
type p2pSearchRequest struct {
	peer       *p2pConnection
	peerHeight int
}

// isBanned tells if we shouldn't have anything to do with the given peer address
func (co *p2pCoordinatorType) isBanned(addr *net.TCPAddr) bool {
	return co.badPeers.Has(addr.String()) || co.badHosts.Has(addr.IP.String())
}

// banPeer temporarily bans the host of a peer which has violated the protocol
func (co *p2pCoordinatorType) banPeer(p2pc *p2pConnection) {
	host, err := remoteHost(p2pc.conn.RemoteAddr())
	if err != nil {
		return
	}
	log.Println("Banning", host, "for protocol violations")
	co.badHosts.Add(host)
}

func (co *p2pCoordinatorType) Run() {
	if tip, err := dbGetTipBlock(); err == nil {
		co.lastTickTipHash, co.lastTickTipHeight = tip.Hash, tip.Height
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg := <-p2pCtrlChannel:
			switch msg.msgType {
			case p2pCtrlSearchForBlocks:
				co.handleSearchForBlocks(msg.payload.(p2pSearchRequest))
			case p2pCtrlConnectPeers:
				co.handleConnectPeers(msg.payload.(p2pConnectPeersRequest))
			}
		case <-ticker.C:
			co.handleTimeTick()
		}
	}
}

// Retrieves block hashes from a node which apparently has a better chain than we do. The range starts a bit
// before our tip, so that the blocks of a competing branch, which forks off below it, are found too. The peer's chain is
// gone through page by page (see syncState).
// ToDo: This is a simplistic version. Make it better by introducing quorums.
func (co *p2pCoordinatorType) handleSearchForBlocks(req p2pSearchRequest) {
	req.peer.startSync(req.peerHeight)
}

// The payload of p2pCtrlConnectPeers: addresses of peers that we have heard of
type p2pConnectPeersRequest struct {
	addresses    []string
	source       string // The address of the peer that has told us, or the kind of source ("seed", "mdns"...)
	allowPrivate bool   // The addresses of private networks are fine
	trusted      bool   // The source is configured by whoever has made the chain (or is the local network), not a peer
	keepPorts    bool   // The addresses are complete, otherwise the ports are replaced with the default one
}

// The number of the outbound connections which we want to have
const p2pTargetOutbound = 8

// The most addresses taken from a list which was not sent by a peer
const p2pMaxTrustedAddresses = 256

// handleConnectPeers puts addresses of peers in the address book, and connects to some of them if we need more peers.
// (The peers don't make us connect to anything: it's the address book that decides.)
func (co *p2pCoordinatorType) handleConnectPeers(req p2pConnectPeersRequest) {
	limit := p2pMaxPeerAddresses // The peer controls this list
	if req.trusted {
		limit = p2pMaxTrustedAddresses
	}
	addresses := req.addresses[:min(len(req.addresses), limit)]
	for _, address := range addresses {
		host, port, ok := splitHostPortDefault(address)
		if !ok {
			continue
		}
		if !req.keepPorts {
			port = DefaultP2PPort // The peers are only looked for on the default port
		}
		if isPrivateHost(host) && !req.allowPrivate {
			continue
		}
		abAdd(net.JoinHostPort(host, strconv.Itoa(port)), req.source)
	}
	co.maintainConnections()
}

// splitHostPortDefault splits "host:port", the port being optional
func splitHostPortDefault(address string) (host string, port int, ok bool) {
	address = strings.TrimSpace(address)
	if h, p, err := net.SplitHostPort(address); err == nil {
		if port, err = strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
			return "", 0, false
		}
		return h, port, h != ""
	}
	address = strings.Trim(address, "[]")
	return address, DefaultP2PPort, address != "" && len(address) <= 255
}

// maintainConnections connects to peers from the address book until there are enough outbound connections.
func (co *p2pCoordinatorType) maintainConnections() {
	need := p2pTargetOutbound - p2pPeers.Count(true) - int(co.pendingDials.Load())
	if need <= 0 {
		return
	}
	groups := make(map[string]bool)
	for _, c := range p2pPeers.Snapshot(false) {
		if c.outbound {
			if host, _, ok := splitHostPortDefault(c.address); ok {
				groups[networkGroup(host)] = true
			}
		}
	}
	delete(groups, "") // The private networks aren't limited
	for _, address := range abSelect(need, groups, func(address string) bool {
		return p2pPeers.HasAddress(address) || co.badPeers.Has(address)
	}) {
		abTry(address)
		co.pendingDials.Add(1)
		go func() {
			p2pc, err := p2pConnectPeer(address)
			co.pendingDials.Add(-1)
			if err != nil {
				return
			}
			p2pc.handleConnection()
		}()
	}
}

// Executed periodically to perform time-dependant actions. Do not rely on the
// time period to be predictable or precise.
func (co *p2pCoordinatorType) handleTimeTick() {
	if tip, err := dbGetTipBlock(); err == nil && tip.Hash != co.lastTickTipHash {
		log.Println("New blocks detected. New max height:", tip.Height)
		from := co.lastTickTipHeight
		if dbGetBlockHashByHeight(co.lastTickTipHeight) != co.lastTickTipHash {
			// The old tip isn't on the best chain any more: the peers need to hear about the blocks it was replaced with
			from = max(0, min(co.lastTickTipHeight, tip.Height)-reorgSearchWindow)
		}
		co.floodPeersWithNewBlocks(from, tip.Height)
		co.lastTickTipHash, co.lastTickTipHeight = tip.Hash, tip.Height
	}
	if time.Since(co.lastReconnectTime) >= 10*time.Minute {
		co.lastReconnectTime = time.Now()
		abPrune()
		p2pPeers.saveConnectablePeers()
		go co.discoverPeers()
	}
	co.maintainConnections()
	p2pPeers.tryPeersConnectable()
}

func (co *p2pCoordinatorType) floodPeersWithNewBlocks(minHeight, maxHeight int) {
	// Only the newest hashes fit in a message. A peer which is further behind gets to the older blocks by asking for them
	// (it does that when it sees hashes of blocks that are too far ahead of its chain).
	minHeight = max(minHeight, maxHeight-p2pMaxBlockHashes+1)
	blockHashes := dbGetHeightHashes(minHeight, maxHeight)
	msg := p2pMsgBlockHashesStruct{
		P2pID:  p2pEphemeralID,
		Root:   chainParams.GenesisBlockHash,
		Msg:    p2pMsgBlockHashes,
		Hashes: blockHashes,
	}
	for _, p2pc := range p2pPeers.Snapshot(true) {
		p2pc.send(msg)
	}
}
