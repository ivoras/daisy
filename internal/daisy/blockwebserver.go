package daisy

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

func blockWebSendBlock(w http.ResponseWriter, r *http.Request) {
	blockHeight, err := strconv.Atoi(r.PathValue("height"))
	if err != nil {
		log.Println("Invalid block height:", r.PathValue("height"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	blockFilename := blockchainGetFilename(blockHeight)
	if _, err := os.Stat(blockFilename); errors.Is(err, fs.ErrNotExist) {
		w.WriteHeader(http.StatusNotFound)
		log.Println("Block file not found:", blockFilename)
		return
	}

	log.Println("HTTP serving block", blockHeight, "to", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/x-sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%08x.db\"", blockHeight))
	http.ServeFile(w, r, blockFilename)
}

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
	mux.HandleFunc("GET /block/{height}", blockWebSendBlock)
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
