package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gorilla/mux"
)

func blockWebSendBlock(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	blockHeight, err := strconv.Atoi(vars["height"])
	if err != nil {
		log.Println(vars)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	blockFilename := blockchainGetFilename(blockHeight)
	if _, err := os.Stat(blockFilename); os.IsNotExist(err) {
		w.WriteHeader(http.StatusNotFound)
		log.Println("Block file not found:", blockFilename)
		return
	}

	log.Println("Serving block", blockHeight, "to", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/x-sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%08x.db\"", blockHeight))
	http.ServeFile(w, r, blockFilename)
	// log.Println("Done serving block", blockHeight)
}

func blockWebSendChainParams(w http.ResponseWriter, r *http.Request) {
	log.Println("Serving chainparams.json to", r.RemoteAddr)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"chainparams.json\"")
	_, err := w.Write(jsonifyWhateverToBytes(chainParams))
	if err != nil {
		log.Println(err)
	}
}

func blockWebServer() {
	r := mux.NewRouter()
	r.HandleFunc("/block/{height}", blockWebSendBlock)
	r.HandleFunc("/chainparams.json", blockWebSendChainParams)

	serverAddress := fmt.Sprintf(":%d", cfg.httpPort)

	log.Println("HTTP listening on", serverAddress)
	err := http.ListenAndServe(serverAddress, r)
	if err != nil {
		panic(err)
	}
}
