package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage/dolt"
)

// Snapshot commands are children of push for discoverability, but deliberately
// bypass its legacy store/Push implementation and post-command maintenance.
var doltSnapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Publish one retained, fixed-R0 native Dolt snapshot",
	Long: `Capture a canonical Git R0 and publish exactly one direct-child Dolt commit.

Use base before the semantic CAS, then prepare with the exact local commit and
root hashes recorded by that CAS. Prepare freezes table bytes in a retained
private stage and returns a native immutable candidate. Publish attempts one
expected-R0 guarded update. If the result is uncertain, use recover against
the same stage; never rebuild or replace the candidate. The configured Dolt
Git remote is the only authority, and stage identity is checked on every step.`,
	Annotations: map[string]string{skipStoreAnnotation: "1"},
}

func snapshotCLIContext(cmd *cobra.Command) (context.Context, error) {
	if readonlyMode || config.GetBool("readonly") {
		return nil, fmt.Errorf("snapshot: remote publication is disabled in readonly mode")
	}
	if config.GetBool("no-push") || isDoltLocalOnly() {
		return nil, fmt.Errorf("snapshot: remote publication is disabled for this workspace")
	}
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return nil, errors.New(activeWorkspaceNotFoundError())
	}
	if err := migrationFreezeGateFor(cmd, "dolt snapshot "+cmd.Name(), beadsDir); err != nil {
		return nil, err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx, nil
}

func snapshotCLIWrite(value any) error {
	return json.NewEncoder(os.Stdout).Encode(value)
}

func init() {
	doltSnapshotCmd.PersistentFlags().String("stage-dir", "", "New or retained private snapshot stage directory")
	doltSnapshotCmd.PersistentFlags().String("remote", "origin", "Persisted Dolt Git authority remote name")
	base := &cobra.Command{
		Use: "base", Short: "Capture the canonical R0 before semantic CAS", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := snapshotCLIContext(cmd)
			if err != nil {
				return err
			}
			stageDir, _ := cmd.Flags().GetString("stage-dir")
			remote, _ := cmd.Flags().GetString("remote")
			if stageDir == "" {
				return fmt.Errorf("snapshot: --stage-dir required")
			}
			beadsDir := selectedDoltBeadsDir()
			if beadsDir == "" {
				return errors.New(activeWorkspaceNotFoundError())
			}
			result, err := dolt.CaptureSnapshotBase(ctx, beadsDir, remote, stageDir)
			if err != nil {
				return err
			}
			return snapshotCLIWrite(map[string]any{"stage_dir": result.StageDir, "authority_ref": result.AuthorityRef, "base": result.Base})
		},
	}
	prepare := &cobra.Command{
		Use: "prepare", Short: "Freeze the selected direct-child Dolt commit and retain its candidate", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := snapshotCLIContext(cmd)
			if err != nil {
				return err
			}
			stageDir, _ := cmd.Flags().GetString("stage-dir")
			remote, _ := cmd.Flags().GetString("remote")
			localCommit, _ := cmd.Flags().GetString("local-commit")
			localRoot, _ := cmd.Flags().GetString("local-root")
			intent, _ := cmd.Flags().GetString("intent-sha256")
			if stageDir == "" {
				return fmt.Errorf("snapshot: --stage-dir required")
			}
			beadsDir := selectedDoltBeadsDir()
			if beadsDir == "" {
				return errors.New(activeWorkspaceNotFoundError())
			}
			result, prepareErr := dolt.PrepareSnapshot(ctx, beadsDir, remote, stageDir, localCommit, localRoot, intent)
			if result.Candidate.PublicationGitOID != "" {
				if err := snapshotCLIWrite(map[string]any{"stage_dir": result.Base.StageDir, "local_commit": result.LocalCommit, "local_root": result.LocalRoot, "candidate": result.Candidate}); err != nil {
					return err
				}
			}
			return prepareErr
		},
	}
	prepare.Flags().String("local-commit", "", "Exact selected local Dolt commit hash after semantic CAS")
	prepare.Flags().String("local-root", "", "Exact selected local Dolt root hash after semantic CAS")
	prepare.Flags().String("intent-sha256", "", "Lowercase SHA256 of the immutable publication intent")
	for _, op := range []struct {
		name, short string
		recoverOnly bool
	}{
		{"publish", "Attempt one native expected-R0 guarded update", false},
		{"recover", "Classify the retained candidate after uncertain publication", true},
	} {
		operation := op
		cmd := &cobra.Command{
			Use: operation.name, Short: operation.short, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, err := snapshotCLIContext(cmd)
				if err != nil {
					return err
				}
				stageDir, _ := cmd.Flags().GetString("stage-dir")
				remote, _ := cmd.Flags().GetString("remote")
				if stageDir == "" {
					return fmt.Errorf("snapshot: --stage-dir required")
				}
				beadsDir := selectedDoltBeadsDir()
				if beadsDir == "" {
					return errors.New(activeWorkspaceNotFoundError())
				}
				outcome, err := dolt.FinishSnapshot(ctx, beadsDir, remote, stageDir, operation.recoverOnly)
				if err != nil {
					return err
				}
				return snapshotCLIWrite(outcome)
			},
		}
		doltSnapshotCmd.AddCommand(cmd)
	}
	capabilities := &cobra.Command{
		Use: "capabilities", Short: "Report native snapshot protocol support", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			probe, _ := cmd.Flags().GetBool("native-snapshot-probe-v1")
			if !probe {
				return fmt.Errorf("snapshot: --native-snapshot-probe-v1 required for capabilities")
			}
			return snapshotCLIWrite(map[string]any{"native_snapshot_v1": true, "authority_ref": "refs/dolt/data", "operations": []string{"base", "prepare", "publish", "recover"}})
		},
	}
	capabilities.Flags().Bool("native-snapshot-probe-v1", false, "Require explicit native snapshot protocol recognition")
	doltSnapshotCmd.AddCommand(capabilities)
	doltSnapshotCmd.AddCommand(base, prepare)
	doltPushCmd.AddCommand(doltSnapshotCmd)
}
