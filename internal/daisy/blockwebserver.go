package daisy

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

// Serves a block by its height (the block of the best chain at that height) or by its hash (any block, including
// the ones which are not on the best chain any more)
func blockWebSendBlock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var blockFilename, downloadName string
	if blockHash.MatchString(id) {
		dbb, onBestChain, err := dbGetAnyBlock(id)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		blockFilename = sideBlockFilename(dbb.Hash)
		if onBestChain {
			blockFilename = blockchainGetFilename(dbb.Height)
		}
		downloadName = fmt.Sprintf("%08x.db", dbb.Height)
	} else {
		blockHeight, err := strconv.Atoi(id)
		if err != nil || blockHeight < 0 {
			log.Println("Invalid block id:", id)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		blockFilename = blockchainGetFilename(blockHeight)
		downloadName = fmt.Sprintf("%08x.db", blockHeight)
	}
	if _, err := os.Stat(blockFilename); errors.Is(err, fs.ErrNotExist) {
		w.WriteHeader(http.StatusNotFound)
		log.Println("Block file not found:", blockFilename)
		return
	}

	log.Println("HTTP serving block", id, "to", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/x-sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", downloadName))
	http.ServeFile(w, r, blockFilename)
}

var blockHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func blockWebSendChainParams(w http.ResponseWriter, r *http.Request) {
	log.Println("HTTP serving chainparams.json to", r.RemoteAddr)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"chainparams.json\"")
	if _, err := w.Write(util.JSONifyBytes(chainParams)); err != nil {
		log.Println(err)
	}
}

// Serves the addresses of the peers that are alive, so that others can find them
func blockWebSendPeers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(util.JSONifyBytes(map[string][]string{"peers": abList(100)})); err != nil {
		log.Println(err)
	}
}

// nodeStatus is what the node says about itself
type nodeStatus struct {
	Version     string `json:"version"`
	Protocol    int    `json:"protocol"`
	MinProtocol int    `json:"min_protocol"`
	Root        string `json:"root"`
	Height      int    `json:"height"`
	TipHash     string `json:"tip_hash,omitempty"`
	ChainScore  int    `json:"chain_score"`
	Peers       struct {
		Inbound  int `json:"inbound"`
		Outbound int `json:"outbound"`
	} `json:"peers"`
	AddressBook struct {
		New   int `json:"new"`
		Tried int `json:"tried"`
	} `json:"address_book"`
	PublicHost string    `json:"public_host,omitempty"` // The address which the peers see us at
	Reachable  bool      `json:"reachable"`             // Peers have been able to connect to our p2p port
	NAT        natStatus `json:"nat"`
}

func currentNodeStatus() nodeStatus {
	var s nodeStatus
	s.Version, s.Protocol, s.MinProtocol = p2pClientVersionString, p2pProtocolVersion, p2pMinProtocolVersion
	s.Root = chainParams.GenesisBlockHash
	s.Height = dbGetBlockchainHeight()
	if tip, err := dbGetTipBlock(); err == nil {
		s.TipHash, s.ChainScore = tip.Hash, tip.TotalScore
	}
	s.Peers.Inbound, s.Peers.Outbound = p2pPeers.Count(false), p2pPeers.Count(true)
	s.AddressBook.New, s.AddressBook.Tried = abCounts()
	s.PublicHost = selfInfo.seenAs()
	if s.PublicHost == "" {
		s.PublicHost = selfInfo.getExternal()
	}
	s.Reachable = selfInfo.isReachable()
	if natMapper != nil {
		s.NAT = natMapper.getStatus()
	}
	return s
}

func blockWebSendStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(util.JSONifyBytes(currentNodeStatus())); err != nil {
		log.Println(err)
	}
}

func blockWebServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /block/{id}", blockWebSendBlock)
	mux.HandleFunc("GET /chainparams.json", blockWebSendChainParams)
	mux.HandleFunc("GET /peers", blockWebSendPeers)
	mux.HandleFunc("GET /status", blockWebSendStatus)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.httpPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Println("HTTP listening on", server.Addr)
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
