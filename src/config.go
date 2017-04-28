package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
)

// DefaultP2PPort is the default TCP port for p2p connections
const DefaultP2PPort = 2017

// DefaultConfigFile is the default configuration filename
const DefaultConfigFile = "/etc/daisy/config.json"

// DefaultDataDir is the default data directory
const DefaultDataDir = "/var/lib/daisy"

var cfg struct {
	configFile string
	P2pPort    int    `json:"p2p_port"`
	DataDir    string `json:"data_dir"`
}

func configInit() {
	// Config file is parsed first
	args := flag.Args()
	for i, arg := range args {
		if arg == "-conf" {
			if i+1 >= len(args) {
				log.Fatal("-conf requires filename argument")
			}
			cfg.configFile = args[i+1]
		}
	}
	if cfg.configFile != "" {
		loadConfigFile()
	}

	flag.IntVar(&cfg.P2pPort, "port", DefaultP2PPort, "P2P port")
	flag.StringVar(&cfg.DataDir, "dir", DefaultDataDir, "Data directory")
	flag.Parse()

	if _, err := os.Stat(cfg.DataDir); err != nil {
		log.Fatal("Data directory", cfg.DataDir, "doesn't exist")
	}
	if cfg.P2pPort < 1 || cfg.P2pPort > 65535 {
		log.Fatal("Invalid TCP port", cfg.P2pPort)
	}
}

func loadConfigFile() {
	data, err := ioutil.ReadFile(cfg.configFile)
	if err != nil {
		log.Fatal(err)
	}
	err = json.Unmarshal(data, &cfg)
	if err != nil {
		log.Fatal(err)
	}
}
