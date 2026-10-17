package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/migration"
)

func TestSnapshotSkipsStoreAndPostCommandMaintenance(t *testing.T) {
	for _, name := range []string{"base", "prepare", "publish", "recover", "capabilities"} {
		cmd, _, err := doltSnapshotCmd.Find([]string{name})
		if err != nil || cmd == nil || cmd.Name() != name {
			t.Fatalf("snapshot %s command unavailable: %v", name, err)
		}
		if !commandOptsOutOfStore(cmd) {
			t.Errorf("snapshot %s unexpectedly opens the legacy store", name)
		}
		if runsPostCommandMaintenanceForCommand(cmd, false) {
			t.Errorf("snapshot %s unexpectedly triggers post-command maintenance", name)
		}
	}
	ordinary := &cobra.Command{Use: "ordinary"}
	if !runsPostCommandMaintenanceForCommand(ordinary, false) {
		t.Fatal("ordinary writable command lost post-command maintenance")
	}
	otherNoStore := &cobra.Command{Use: "other", Annotations: map[string]string{skipStoreAnnotation: "1"}}
	if !runsPostCommandMaintenanceForCommand(otherNoStore, false) {
		t.Fatal("existing annotated command behavior changed")
	}
}

func TestSnapshotCapabilitiesProbeCannotInvokeLegacyPush(t *testing.T) {
	// A pre-adapter binary has `push` but no `snapshot` subcommand. The unique
	// probe flag must fail Cobra flag parsing before its legacy RunE executes.
	legacyPushCalled := false
	legacy := &cobra.Command{
		Use: "push",
		RunE: func(*cobra.Command, []string) error {
			legacyPushCalled = true
			return nil
		},
	}
	legacy.SetOut(io.Discard)
	legacy.SetErr(io.Discard)
	legacy.SetArgs([]string{"snapshot", "capabilities", "--native-snapshot-probe-v1"})
	err := legacy.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag") || legacyPushCalled {
		t.Fatalf("legacy push probe result = %v, legacy called = %t", err, legacyPushCalled)
	}
	capabilities, _, err := doltSnapshotCmd.Find([]string{"capabilities"})
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilities.RunE(capabilities, nil); err == nil || !strings.Contains(err.Error(), "--native-snapshot-probe-v1 required") {
		t.Fatalf("unguarded native capabilities probe: %v", err)
	}
}

func TestSnapshotPublicationCommandsHonorReadonlyAndFreeze(t *testing.T) {
	root := t.TempDir()
	beadsDir := filepath.Join(root, ".beads")
	if err := os.Mkdir(beadsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := configfile.DefaultConfig().Save(beadsDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", beadsDir)
	oldReadonly := readonlyMode
	oldReadonlyConfig := config.GetBool("readonly")
	oldNoPush := config.GetBool("no-push")
	oldLocalOnly := config.GetBool("dolt.local-only")
	t.Cleanup(func() {
		readonlyMode = oldReadonly
		config.Set("readonly", oldReadonlyConfig)
		config.Set("no-push", oldNoPush)
		config.Set("dolt.local-only", oldLocalOnly)
	})
	config.Set("no-push", false)
	config.Set("readonly", false)
	config.Set("dolt.local-only", false)
	for _, name := range []string{"base", "prepare", "publish", "recover"} {
		cmd, _, err := doltSnapshotCmd.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		readonlyMode = true
		if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "readonly mode") {
			t.Fatalf("snapshot %s bypassed readonly admission: %v", name, err)
		}
	}
	readonlyMode = false
	marker := filepath.Join(root, migration.FileName)
	if err := os.WriteFile(marker, []byte("snapshot test freeze"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(migration.EnvFreezeFile, marker)
	for _, name := range []string{"base", "prepare", "publish", "recover"} {
		cmd, _, err := doltSnapshotCmd.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		err = cmd.RunE(cmd, nil)
		if code, ok := exitCodeFromError(err); !ok || code != ExitMigrationFrozen {
			t.Fatalf("snapshot %s bypassed selected-workspace freeze: %v", name, err)
		}
	}
	if entries, err := os.ReadDir(beadsDir); err != nil || len(entries) != 1 || entries[0].Name() != configfile.ConfigFileName {
		t.Fatalf("refused snapshot operation touched workspace: entries=%v err=%v", entries, err)
	}
}
