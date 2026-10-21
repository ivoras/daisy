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

func blockWebServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /block/{id}", blockWebSendBlock)
	mux.HandleFunc("GET /chainparams.json", blockWebSendChainParams)

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
