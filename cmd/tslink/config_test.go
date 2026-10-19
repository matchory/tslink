package main

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// pluginConfig is the part of docker/config.json the plugin's start relies on.
type pluginConfig struct {
	Mounts []struct {
		Name        string   `json:"name"`
		Source      string   `json:"source"`
		Destination string   `json:"destination"`
		Type        string   `json:"type"`
		Settable    []string `json:"settable"`
		Options     []string `json:"options"`
	} `json:"mounts"`
	Env []struct {
		Name     string   `json:"name"`
		Value    string   `json:"value"`
		Settable []string `json:"settable"`
	} `json:"env"`
	PropagatedMount *string `json:"propagatedMount"`
}

// TestPluginConfigMounts pins what prepareDataDir expects of
// docker/config.json. docker plugin upgrade replaces the mounts but keeps the
// environment of the first install, so TS_DATA_DIR and TS_SHARED_DIR must
// keep their values, and only the mounts may change.
func TestPluginConfigMounts(t *testing.T) {
	raw, err := os.ReadFile("../../docker/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg pluginConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PropagatedMount != nil {
		t.Errorf("propagatedMount = %q, want none", *cfg.PropagatedMount)
	}

	env := map[string]string{}
	for _, e := range cfg.Env {
		env[e.Name] = e.Value
		if (e.Name == "TS_DATA_DIR" || e.Name == "TS_SHARED_DIR") && len(e.Settable) > 0 {
			t.Errorf("%s is settable", e.Name)
		}
	}
	if env["TS_DATA_DIR"] != "/data" || env["TS_SHARED_DIR"] != "/shared" {
		t.Errorf("TS_DATA_DIR=%q, TS_SHARED_DIR=%q; want /data and /shared",
			env["TS_DATA_DIR"], env["TS_SHARED_DIR"])
	}

	var varLib, shared bool
	for _, m := range cfg.Mounts {
		switch m.Destination {
		case "/data":
			t.Errorf("mount %s at /data: the plugin mounts its data directory itself", m.Name)
		case hostVarLib:
			varLib = true
			if m.Source != "/var/lib" || m.Type != "bind" ||
				!slices.Equal(m.Options, []string{"rbind", "rslave", "rw"}) || len(m.Settable) > 0 {
				t.Errorf("mount at %s = %+v, want /var/lib, bind, rbind,rslave,rw, not settable",
					hostVarLib, m)
			}
		case "/shared":
			shared = true
			if m.Name != "shared" || m.Source != "/var/lib" ||
				!slices.Equal(m.Settable, []string{"source"}) {
				t.Errorf("mount at /shared = %+v, want shared, source /var/lib, settable", m)
			}
		}
	}
	if !varLib || !shared {
		t.Errorf("mounts at %s: %v, at /shared: %v; want both", hostVarLib, varLib, shared)
	}
}
