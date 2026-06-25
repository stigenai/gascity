package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestConfigStateConstructorsSetDoltModeForServerBackedEndpoints(t *testing.T) {
	t.Run("desired city managed runtime", func(t *testing.T) {
		state := desiredCityDoltConfigState(t.TempDir(), config.DoltConfig{}, "gc")

		if state.EndpointOrigin != contract.EndpointOriginManagedCity {
			t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginManagedCity)
		}
		if state.DoltHost != "" || state.DoltPort != "" {
			t.Fatalf("managed city host/port = %q/%q, want empty managed runtime coordinates", state.DoltHost, state.DoltPort)
		}
		assertDoltModeServer(t, state, "managed city runtime")
	})

	t.Run("desired city external endpoint", func(t *testing.T) {
		state := desiredCityDoltConfigState(t.TempDir(), config.DoltConfig{
			Host: "db.example.test",
			Port: 4406,
		}, "gc")

		if state.EndpointOrigin != contract.EndpointOriginCityCanonical {
			t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginCityCanonical)
		}
		if state.DoltHost != "db.example.test" || state.DoltPort != "4406" {
			t.Fatalf("city external host/port = %q/%q, want db.example.test/4406", state.DoltHost, state.DoltPort)
		}
		assertDoltModeServer(t, state, "city external endpoint")
	})

	t.Run("desired rig explicit endpoint", func(t *testing.T) {
		cityPath := t.TempDir()
		rigPath := filepath.Join(cityPath, "frontend")
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}

		state := desiredRigDoltConfigState(cityPath, config.Rig{
			Name:     "frontend",
			Path:     rigPath,
			Prefix:   "fe",
			DoltHost: "rig-db.example.test",
			DoltPort: "4407",
		}, contract.ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: contract.EndpointOriginManagedCity,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltMode:       "server",
		})

		if state.EndpointOrigin != contract.EndpointOriginExplicit {
			t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginExplicit)
		}
		if state.DoltHost != "rig-db.example.test" || state.DoltPort != "4407" {
			t.Fatalf("rig explicit host/port = %q/%q, want rig-db.example.test/4407", state.DoltHost, state.DoltPort)
		}
		assertDoltModeServer(t, state, "rig explicit endpoint")
	})

	t.Run("inherited rig endpoint", func(t *testing.T) {
		state := inheritedRigDoltConfigState(t.TempDir(), "fe", contract.ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: contract.EndpointOriginCityCanonical,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "db.example.test",
			DoltPort:       "4406",
			DoltUser:       "root",
			DoltMode:       "server",
		})

		if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
			t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginInheritedCity)
		}
		if state.DoltHost != "db.example.test" || state.DoltPort != "4406" || state.DoltUser != "root" {
			t.Fatalf("inherited rig target = %q/%q user %q, want db.example.test/4406 user root", state.DoltHost, state.DoltPort, state.DoltUser)
		}
		assertDoltModeServer(t, state, "inherited rig endpoint")
	})

	t.Run("requested rig self endpoint", func(t *testing.T) {
		state := requestedRigEndpointState(config.Rig{
			Name:   "frontend",
			Path:   t.TempDir(),
			Prefix: "fe",
		}, contract.ConfigState{}, contract.ConfigState{}, rigEndpointOptions{
			Self: true,
			Port: "28232",
		})

		if state.EndpointOrigin != contract.EndpointOriginExplicit {
			t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginExplicit)
		}
		if state.DoltHost != "127.0.0.1" || state.DoltPort != "28232" {
			t.Fatalf("requested self host/port = %q/%q, want 127.0.0.1/28232", state.DoltHost, state.DoltPort)
		}
		assertDoltModeServer(t, state, "requested rig self endpoint")
	})

	t.Run("requested rig external endpoint", func(t *testing.T) {
		state := requestedRigEndpointState(config.Rig{
			Name:   "frontend",
			Path:   t.TempDir(),
			Prefix: "fe",
		}, contract.ConfigState{}, contract.ConfigState{}, rigEndpointOptions{
			External: true,
			Host:     "rig-db.example.test",
			Port:     "4407",
		})

		if state.EndpointOrigin != contract.EndpointOriginExplicit {
			t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginExplicit)
		}
		if state.DoltHost != "rig-db.example.test" || state.DoltPort != "4407" {
			t.Fatalf("requested external host/port = %q/%q, want rig-db.example.test/4407", state.DoltHost, state.DoltPort)
		}
		assertDoltModeServer(t, state, "requested rig external endpoint")
	})

	t.Run("requested rig inherited endpoint", func(t *testing.T) {
		state := requestedRigEndpointState(config.Rig{
			Name:   "frontend",
			Path:   t.TempDir(),
			Prefix: "fe",
		}, contract.ConfigState{}, contract.ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: contract.EndpointOriginCityCanonical,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "db.example.test",
			DoltPort:       "4406",
			DoltMode:       "server",
		}, rigEndpointOptions{
			Inherit: true,
		})

		if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
			t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginInheritedCity)
		}
		if state.DoltHost != "db.example.test" || state.DoltPort != "4406" {
			t.Fatalf("requested inherited host/port = %q/%q, want db.example.test/4406", state.DoltHost, state.DoltPort)
		}
		assertDoltModeServer(t, state, "requested rig inherited endpoint")
	})
}

func TestConfigStateConstructorsLeaveDoltModeEmptyWithoutKnownDoltEndpoint(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	rig := config.Rig{Name: "frontend", Path: rigPath, Prefix: "fe"}
	cityState := contract.ConfigState{IssuePrefix: "gc"}

	tests := []struct {
		name  string
		state contract.ConfigState
	}{
		{
			name:  "desired rig inherits empty city endpoint",
			state: desiredRigDoltConfigState(cityPath, rig, cityState),
		},
		{
			name: "requested rig inherits empty city endpoint",
			state: requestedRigEndpointState(rig, contract.ConfigState{}, cityState, rigEndpointOptions{
				Inherit: true,
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.state.DoltMode != "" {
				t.Fatalf("DoltMode = %q, want empty when no managed Dolt and no Dolt host/port are known", tt.state.DoltMode)
			}
			if tt.state.DoltHost != "" || tt.state.DoltPort != "" {
				t.Fatalf("Dolt host/port = %q/%q, want empty", tt.state.DoltHost, tt.state.DoltPort)
			}

			dir := t.TempDir()
			configPath := filepath.Join(dir, ".beads", "config.yaml")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, configPath, tt.state); err != nil {
				t.Fatalf("EnsureCanonicalConfig: %v", err)
			}
			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if strings.Contains(text, "dolt.mode:") || strings.Contains(text, "\n  mode:") {
				t.Fatalf("config unexpectedly forced dolt.mode without a known Dolt endpoint:\n%s", data)
			}
			readState, ok, err := contract.ReadConfigState(fsys.OSFS{}, configPath)
			if err != nil {
				t.Fatalf("ReadConfigState: %v", err)
			}
			if !ok {
				t.Fatal("ReadConfigState() = !ok, want written config")
			}
			if readState.DoltMode != "" {
				t.Fatalf("written DoltMode = %q, want empty", readState.DoltMode)
			}
		})
	}
}

func assertDoltModeServer(t *testing.T, state contract.ConfigState, label string) {
	t.Helper()
	if state.DoltMode != "server" {
		t.Fatalf("%s DoltMode = %q, want server", label, state.DoltMode)
	}
}
