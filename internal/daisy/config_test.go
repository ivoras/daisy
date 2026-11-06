package daisy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigFile(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{"p2p_port":12017,"http_port":12018,"data_dir":"/x","upnp":true,"mdns":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.configFile = file
	loadConfigFile()
	if cfg.P2pPort != 12017 || cfg.HTTPPort != 12018 || cfg.DataDir != "/x" || !cfg.UPnP || !cfg.MDNS {
		t.Errorf("loaded %+v", cfg)
	}
}
