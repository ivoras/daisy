package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/user"
)

// DefaultP2PPort is the default TCP port for p2p connections
const DefaultP2PPort = 2017

// DefaultConfigFile is the default configuration filename
const DefaultConfigFile = "/etc/daisy/config.json"

// DefaultDataDir is the default data directory
const DefaultDataDir = ".daisy"

var cfg struct {
	configFile string
	P2pPort    int    `json:"p2p_port"`
	DataDir    string `json:"data_dir"`
	showHelp   bool
}

// Initialises defaults, parses command line
func configInit() {
	u, err := user.Current()
	if err != nil {
		log.Panicln(err)
	}
	cfg.DataDir = fmt.Sprintf("%s/%s", u.HomeDir, DefaultDataDir)

	// Init defaults
	cfg.P2pPort = DefaultP2PPort

	// Config file is parsed first
	for i, arg := range os.Args {
		if arg == "-conf" {
			if i+1 >= len(os.Args) {
				log.Fatal("-conf requires filename argument")
			}
			cfg.configFile = os.Args[i+1]
		}
	}
	if cfg.configFile != "" {
		loadConfigFile()
	}

	// Then override the configuration with command-line flags
	flag.IntVar(&cfg.P2pPort, "port", cfg.P2pPort, "P2P port")
	flag.StringVar(&cfg.DataDir, "dir", cfg.DataDir, "Data directory")
	flag.BoolVar(&cfg.showHelp, "help", false, "Shows CLI usage information")
	flag.Parse()

	if cfg.showHelp {
		actionHelp()
		os.Exit(0)
	}

	if _, err := os.Stat(cfg.DataDir); err != nil {
		log.Println("Data directory", cfg.DataDir, "doesn't exist, creating.")
		err = os.Mkdir(cfg.DataDir, 0700)
		if err != nil {
			log.Panicln(err)
		}
	}
	if cfg.P2pPort < 1 || cfg.P2pPort > 65535 {
		log.Fatal("Invalid TCP port", cfg.P2pPort)
	}
}

// Loads the JSON config file.
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
