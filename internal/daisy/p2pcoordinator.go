package daisy

import (
	"fmt"
	"log"
	"net"
	"slices"
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
	timeTicks                chan int
	lastTickBlockchainHeight int
	recentlyRequestedBlocks  *util.StringSetWithExpiry
	lastReconnectTime        time.Time
	badPeers                 *util.StringSetWithExpiry // Addresses ("ip:port") we have dialed and shouldn't dial again
	badHosts                 *util.StringSetWithExpiry // Hosts (IP addresses) whose connections are refused and not dialed
}

// XXX: singletons in go?
var p2pCoordinator = p2pCoordinatorType{
	recentlyRequestedBlocks: util.NewStringSetWithExpiry(5 * time.Second),
	lastReconnectTime:       time.Now(),
	timeTicks:               make(chan int),
	badPeers:                util.NewStringSetWithExpiry(15 * time.Minute),
	badHosts:                util.NewStringSetWithExpiry(15 * time.Minute),
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
	co.lastTickBlockchainHeight = dbGetBlockchainHeight()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg := <-p2pCtrlChannel:
			switch msg.msgType {
			case p2pCtrlSearchForBlocks:
				co.handleSearchForBlocks(msg.payload.(*p2pConnection))
			case p2pCtrlConnectPeers:
				co.handleConnectPeers(msg.payload.([]string))
			}
		case <-ticker.C:
			co.handleTimeTick()
		}
	}
}

// Retrieves block hashes from a node which apparently has more blocks than we do.
// ToDo: This is a simplistic version. Make it better by introducing quorums.
func (co *p2pCoordinatorType) handleSearchForBlocks(p2pcStart *p2pConnection) {
	msg := p2pMsgGetBlockHashesStruct{
		P2pID:          p2pEphemeralID,
		Root:           chainParams.GenesisBlockHash,
		Msg:            p2pMsgGetBlockHashes,
		MinBlockHeight: dbGetBlockchainHeight(),
		MaxBlockHeight: p2pcStart.chainHeight,
	}
	log.Printf("Searching for blocks from %d to %d", msg.MinBlockHeight, msg.MaxBlockHeight)
	p2pcStart.send(msg)
}

func (co *p2pCoordinatorType) handleConnectPeers(addresses []string) {
	localAddresses := util.LocalAddresses()

	// The peer controls this list, and every address means a (slow) connection attempt
	addresses = addresses[:min(len(addresses), p2pMaxPeerAddresses)]

	for _, address := range addresses {
		host, _, err := util.SplitAddress(address)
		if err != nil {
			log.Println(address, err)
			continue
		}
		canonicalAddress := fmt.Sprintf("%s:%d", host, DefaultP2PPort)
		if p2pPeers.HasAddress(canonicalAddress) {
			continue
		}
		addr, err := net.ResolveTCPAddr("tcp", canonicalAddress)
		if err != nil {
			continue
		}
		if co.isBanned(addr) {
			continue
		}
		if slices.Contains(localAddresses, addr.IP.String()) {
			continue
		}
		// Detect if there's a canonical peer on the other side, somewhat brute-forceish
		conn, err := p2pDialer.Dial("tcp", addr.String())
		if err != nil {
			continue
		}
		p2pc, err := p2pSetupPeer(addr.String(), conn, true)
		if err != nil {
			log.Println("handleConnectPeers:", err)
			continue
		}
		go p2pc.handleConnection()
		log.Println("Detected canonical peer at", canonicalAddress)
		dbSavePeer(canonicalAddress)
	}
}

// Executed periodically to perform time-dependant actions. Do not rely on the
// time period to be predictable or precise.
func (co *p2pCoordinatorType) handleTimeTick() {
	newHeight := dbGetBlockchainHeight()
	if newHeight > co.lastTickBlockchainHeight {
		log.Println("New blocks detected. New max height:", newHeight)
		co.floodPeersWithNewBlocks(co.lastTickBlockchainHeight, newHeight)
		co.lastTickBlockchainHeight = newHeight
	}
	if time.Since(co.lastReconnectTime) >= 10*time.Minute {
		co.lastReconnectTime = time.Now()
		p2pPeers.saveConnectablePeers()
		co.connectDbPeers()
	}
	p2pPeers.tryPeersConnectable()
}

func (co *p2pCoordinatorType) floodPeersWithNewBlocks(minHeight, maxHeight int) {
	blockHashes := dbGetHeightHashes(minHeight, maxHeight)
	msg := p2pMsgBlockHashesStruct{
		P2pID:  p2pEphemeralID,
		Root:   chainParams.GenesisBlockHash,
		Msg:    p2pMsgBlockHashes,
		Hashes: blockHashes,
	}
	for _, p2pc := range p2pPeers.Snapshot() {
		p2pc.send(msg)
	}
}

func (co *p2pCoordinatorType) connectDbPeers() {
	peers := dbGetSavedPeers()
	for peer := range peers {
		if p2pPeers.HasAddress(peer) {
			continue
		}
		if co.badPeers.Has(peer) {
			continue
		}
		p2pc, err := p2pConnectPeer(peer)
		if err != nil {
			continue
		}
		go p2pc.handleConnection()
	}
}
