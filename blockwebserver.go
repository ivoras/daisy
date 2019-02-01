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

	log.Println("Serving block", blockHeight)
	w.Header().Set("Content-Type", "application/x-sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%08x.db\"", blockHeight))
	http.ServeFile(w, r, blockFilename)
}

func blockWebServer() {
	r := mux.NewRouter()
	r.HandleFunc("/block/{height}", blockWebSendBlock)

	serverAddress := fmt.Sprintf(":%d", DefaultBlockWebServerPort)

	log.Println("HTTP listening on", serverAddress)
	err := http.ListenAndServe(serverAddress, r)
	if err != nil {
		panic(err)
	}
}
