package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestCmdHookBindsProjectedRuntimeRig(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rig")
	fakeBin := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(rigDir, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","project_id":"infra-blocks"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[rigs]]
name = "specs"
prefix = "sp"

[[rigs]]
name = "infra-blocks"
prefix = "ib"

[[agent]]
name = "review-pre"
dir = "infra-blocks"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'pwd=%s\\nstore_root=%s\\nstore_scope=%s\\nprefix=%s\\nrig=%s\\nrig_root=%s\\n' \"$PWD\" \"$GC_STORE_ROOT\" \"$GC_STORE_SCOPE\" \"$GC_BEADS_PREFIX\" \"$GC_RIG\" \"$GC_RIG_ROOT\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "infra-blocks/ib-ops--review-pre-st-abc12")
	t.Setenv("GC_ALIAS", "infra-blocks/ib-ops--review-pre-st-abc12")
	t.Setenv("GC_TEMPLATE", "infra-blocks/review-pre")
	t.Setenv("GC_SESSION_ID", "ib-session")
	t.Setenv("GC_SESSION_NAME", "infra-blocks/ib-ops--review-pre-st-abc12")
	t.Setenv("GC_RIG", "infra-blocks")
	t.Setenv("GC_RIG_ROOT", rigDir)
	t.Setenv("GC_STORE_ROOT", rigDir)
	t.Setenv("GC_STORE_SCOPE", "rig")
	t.Setenv("GC_BEADS_PREFIX", "ib")

	var stdout, stderr bytes.Buffer
	if code := cmdHook(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"pwd=" + rigDir,
		"store_root=" + rigDir,
		"store_scope=rig",
		"prefix=ib",
		"rig=infra-blocks",
		"rig_root=" + rigDir,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want %q", out, want)
		}
	}
}

func TestCmdHookHonorsExternalSiteRigBinding(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	rigDir := t.TempDir()
	fakeBin := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(rigDir, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","project_id":"specs"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[rigs]]
name = "specs"
prefix = "sp"

[[agent]]
name = "plan-draft"
dir = "specs"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	relativeRigDir, err := filepath.Rel(cityDir, rigDir)
	if err != nil {
		t.Fatal(err)
	}
	siteToml := fmt.Sprintf("[[rig]]\nname = \"specs\"\npath = %q\n", relativeRigDir)
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte(siteToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'pwd=%s\\nstore_root=%s\\nstore_scope=%s\\nprefix=%s\\nrig=%s\\nrig_root=%s\\nsession_id=%s\\n' \"$PWD\" \"$GC_STORE_ROOT\" \"$GC_STORE_SCOPE\" \"$GC_BEADS_PREFIX\" \"$GC_RIG\" \"$GC_RIG_ROOT\" \"$GC_SESSION_ID\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "specs/speckit--plan-draft-st-abc12")
	t.Setenv("GC_ALIAS", "specs/speckit--plan-draft-st-abc12")
	t.Setenv("GC_TEMPLATE", "specs/plan-draft")
	t.Setenv("GC_SESSION_ID", "spec-session")
	t.Setenv("GC_SESSION_NAME", "specs/speckit--plan-draft-st-abc12")
	t.Setenv("GC_RIG", "specs")
	t.Setenv("GC_RIG_ROOT", rigDir)
	t.Setenv("GC_STORE_ROOT", rigDir)
	t.Setenv("GC_STORE_SCOPE", "rig")
	t.Setenv("GC_BEADS_PREFIX", "sp")

	var stdout, stderr bytes.Buffer
	if code := cmdHook(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"pwd=" + rigDir,
		"store_root=" + rigDir,
		"store_scope=rig",
		"prefix=sp",
		"rig=specs",
		"rig_root=" + rigDir,
		"session_id=spec-session",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want %q", out, want)
		}
	}
}

func TestBindProjectedRuntimeRig(t *testing.T) {
	makeRigRoot := func(t *testing.T, cityDir string) string {
		t.Helper()
		rigDir := filepath.Join(cityDir, "rig")
		if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"backend":"dolt"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return rigDir
	}
	baseConfig := func() *config.City {
		return &config.City{Rigs: []config.Rig{
			{Name: "specs", Prefix: "sp"},
			{Name: "infra-blocks", Prefix: "ib"},
		}}
	}
	setValidEnv := func(t *testing.T, rigDir string) {
		t.Helper()
		clearGCEnv(t)
		t.Setenv("GC_SESSION_ID", "ib-session")
		t.Setenv("GC_RIG", "infra-blocks")
		t.Setenv("GC_RIG_ROOT", rigDir)
		t.Setenv("GC_STORE_ROOT", rigDir)
		t.Setenv("GC_STORE_SCOPE", "rig")
		t.Setenv("GC_BEADS_PREFIX", "ib")
	}

	t.Run("binds only matching pathless declared rig", func(t *testing.T) {
		cityDir := t.TempDir()
		rigDir := makeRigRoot(t, cityDir)
		setValidEnv(t, rigDir)
		cfg := baseConfig()

		if err := bindProjectedRuntimeRig(cityDir, cfg); err != nil {
			t.Fatalf("bindProjectedRuntimeRig() error = %v", err)
		}
		if got := cfg.Rigs[0].Path; got != "" {
			t.Fatalf("specs path = %q, want empty", got)
		}
		if got := cfg.Rigs[1].Path; got != rigDir {
			t.Fatalf("infra-blocks path = %q, want %q", got, rigDir)
		}
	})

	t.Run("accepts exact configured path outside city", func(t *testing.T) {
		cityDir := t.TempDir()
		rigDir := makeRigRoot(t, t.TempDir())
		setValidEnv(t, rigDir)
		cfg := baseConfig()
		cfg.Rigs[1].Path = rigDir

		if err := bindProjectedRuntimeRig(cityDir, cfg); err != nil {
			t.Fatalf("bindProjectedRuntimeRig() error = %v", err)
		}
		if got := cfg.Rigs[1].Path; got != rigDir {
			t.Fatalf("infra-blocks path = %q, want %q", got, rigDir)
		}
	})

	t.Run("accepts configured symlink alias for the same external path", func(t *testing.T) {
		cityDir := t.TempDir()
		externalDir := t.TempDir()
		rigDir := makeRigRoot(t, externalDir)
		alias := filepath.Join(t.TempDir(), "rig-alias")
		if err := os.Symlink(rigDir, alias); err != nil {
			t.Fatal(err)
		}
		setValidEnv(t, rigDir)
		cfg := baseConfig()
		cfg.Rigs[1].Path = alias

		if err := bindProjectedRuntimeRig(cityDir, cfg); err != nil {
			t.Fatalf("bindProjectedRuntimeRig() error = %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(t *testing.T, cityDir, rigDir string, cfg *config.City)
		want   string
	}{
		{
			name: "requires managed session",
			mutate: func(t *testing.T, _, _ string, _ *config.City) {
				t.Setenv("GC_SESSION_ID", "")
				t.Setenv("GC_SESSION_NAME", "")
			},
			want: "managed session",
		},
		{
			name: "rejects undeclared rig",
			mutate: func(t *testing.T, _, _ string, _ *config.City) {
				t.Setenv("GC_RIG", "unknown")
			},
			want: `rig "unknown" is not declared`,
		},
		{
			name: "rejects path outside city",
			mutate: func(t *testing.T, _, _ string, _ *config.City) {
				outside := t.TempDir()
				if err := os.MkdirAll(filepath.Join(outside, ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(outside, ".beads", "metadata.json"), []byte(`{}`), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GC_RIG_ROOT", outside)
				t.Setenv("GC_STORE_ROOT", outside)
			},
			want: "inside city root",
		},
		{
			name: "rejects in-city symlink escaping outside city",
			mutate: func(t *testing.T, cityDir, _ string, _ *config.City) {
				outside := makeRigRoot(t, t.TempDir())
				alias := filepath.Join(cityDir, "rig-alias")
				if err := os.Symlink(outside, alias); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GC_RIG_ROOT", alias)
				t.Setenv("GC_STORE_ROOT", alias)
			},
			want: "inside city root",
		},
		{
			name: "rejects missing metadata",
			mutate: func(t *testing.T, cityDir, _ string, _ *config.City) {
				empty := filepath.Join(cityDir, "empty-rig")
				if err := os.MkdirAll(empty, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GC_RIG_ROOT", empty)
				t.Setenv("GC_STORE_ROOT", empty)
			},
			want: "metadata.json",
		},
		{
			name: "rejects store root mismatch",
			mutate: func(t *testing.T, cityDir, _ string, _ *config.City) {
				t.Setenv("GC_STORE_ROOT", cityDir)
			},
			want: "GC_STORE_ROOT",
		},
		{
			name: "rejects prefix mismatch",
			mutate: func(t *testing.T, _, _ string, _ *config.City) {
				t.Setenv("GC_BEADS_PREFIX", "wrong")
			},
			want: "GC_BEADS_PREFIX",
		},
		{
			name: "does not override authored path",
			mutate: func(_ *testing.T, cityDir, _ string, cfg *config.City) {
				cfg.Rigs[1].Path = filepath.Join(cityDir, "authored")
			},
			want: "already has configured path",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			rigDir := makeRigRoot(t, cityDir)
			setValidEnv(t, rigDir)
			cfg := baseConfig()
			tc.mutate(t, cityDir, rigDir, cfg)

			err := bindProjectedRuntimeRig(cityDir, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("bindProjectedRuntimeRig() error = %v, want containing %q", err, tc.want)
			}
			if got := cfg.Rigs[1].Path; got != "" && !strings.Contains(tc.name, "authored") {
				t.Fatalf("infra-blocks path changed on failure: %q", got)
			}
		})
	}
}

func TestBindProjectedRuntimeRigIgnoresCityScopedSession(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_SESSION_ID", "city-session")
	cfg := &config.City{Rigs: []config.Rig{{Name: "infra-blocks", Prefix: "ib"}}}
	if err := bindProjectedRuntimeRig(t.TempDir(), cfg); err != nil {
		t.Fatalf("bindProjectedRuntimeRig() error = %v", err)
	}
	if got := fmt.Sprint(cfg.Rigs[0].Path); got != "" {
		t.Fatalf("rig path = %q, want empty", got)
	}
}
