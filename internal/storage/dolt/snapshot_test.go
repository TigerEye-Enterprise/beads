package dolt

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/blobstore"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/steveyegge/beads/internal/configfile"
)

func snapshotTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- test-only disposable Git repository
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=NUL")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSnapshotReceiptExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.json")
	if err := os.WriteFile(path+".tmp", []byte("orphaned prior attempt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := snapshotWriteExclusive(path, map[string]string{"r0": "first"}); err != nil {
		t.Fatal(err)
	}
	if err := snapshotWriteExclusive(path, map[string]string{"r0": "replacement"}); err == nil {
		t.Fatal("retained base was overwritten")
	}
	var got map[string]string
	if err := snapshotRead(path, &got); err != nil || got["r0"] != "first" {
		t.Fatalf("retained base changed: %v, %v", got, err)
	}
}

func TestSnapshotStageBindsPersistedAuthority(t *testing.T) {
	t.Setenv("BEADS_DOLT_DATA_DIR", "")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "NUL")
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "authority.git")
	remoteURL := filepath.ToSlash(remote)
	if err := os.Mkdir(remote, 0700); err != nil {
		t.Fatal(err)
	}
	snapshotTestGit(t, remote, "init", "--bare")
	snapshotTestGit(t, remote, "config", "--local", "core.hooksPath", filepath.Join(remote, "hooks"))
	writer := filepath.Join(root, "writer")
	if err := os.Mkdir(writer, 0700); err != nil {
		t.Fatal(err)
	}
	snapshotTestGit(t, writer, "init")
	snapshotTestGit(t, writer, "config", "--local", "core.hooksPath", filepath.Join(writer, ".git", "hooks"))
	snapshotTestGit(t, writer, "config", "--local", "user.name", "Snapshot Test")
	snapshotTestGit(t, writer, "config", "--local", "user.email", "snapshot@test.invalid")
	if err := os.WriteFile(filepath.Join(writer, "manifest"), []byte("5:7.18:lock:root:gen:table:1"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshotTestGit(t, writer, "add", "manifest")
	snapshotTestGit(t, writer, "commit", "-m", "R0")
	snapshotTestGit(t, writer, "push", remote, "HEAD:refs/dolt/data")
	beadsDir := filepath.Join(root, ".beads")
	dataDir := filepath.Join(root, "dolt")
	localDir := filepath.Join(dataDir, "snapshot")
	if err := os.MkdirAll(filepath.Join(localDir, ".dolt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(beadsDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := configfile.DefaultConfig()
	cfg.DoltDatabase = "snapshot"
	cfg.DoltDataDir = filepath.Join("..", "dolt")
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(localDir, ".dolt", "repo_state.json")
	writeRemote := func(url string) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"remotes": map[string]any{"origin": map[string]string{"url": url}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeRemote(remoteURL)
	stage := filepath.Join(root, "stage")
	base, err := CaptureSnapshotBase(ctx, beadsDir, "origin", stage)
	if err != nil {
		t.Fatal(err)
	}
	if base.Base.R0GitOID == "" || base.Base.ManifestVersion == "" {
		t.Fatalf("R0 and manifest version not captured: %+v", base.Base)
	}
	if _, _, err := snapshotOpenStage(ctx, beadsDir, "origin", stage); err != nil {
		t.Fatalf("unchanged authority rejected: %v", err)
	}
	if _, _, err := snapshotOpenStage(ctx, beadsDir, "other", stage); err == nil {
		t.Fatal("stage accepted a different selected remote")
	}
	placeholder := hash.Of([]byte("recorded-cas-placeholder")).String()
	if _, err := PrepareSnapshot(ctx, beadsDir, "other", stage, placeholder, placeholder, strings.Repeat("a", 64)); err == nil {
		t.Fatal("preparation accepted a different selected remote")
	}
	if _, err := os.Stat(filepath.Join(stage, "prepare.started")); !os.IsNotExist(err) {
		t.Fatalf("remote mismatch started preparation: %v", err)
	}
	if err := os.WriteFile(filepath.Join(writer, "manifest"), []byte("5:7.18:lock:new-root:gen:table:1"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshotTestGit(t, writer, "add", "manifest")
	snapshotTestGit(t, writer, "commit", "-m", "R1")
	snapshotTestGit(t, writer, "push", remote, "HEAD:refs/dolt/data")
	pinned, err := snapshotPinnedBaselineStore(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := blobstore.GetBytes(ctx, pinned, "manifest", blobstore.AllRange)
	if err != nil || string(manifest) != "5:7.18:lock:root:gen:table:1" {
		t.Fatalf("pinned R0 followed later authority commit: %q, %v", manifest, err)
	}
	if _, err := PrepareSnapshot(ctx, beadsDir, "origin", stage, placeholder, placeholder, strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed canonical ref did not reject saved R0: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stage, "prepare.started")); !os.IsNotExist(err) {
		t.Fatalf("stale R0 started preparation: %v", err)
	}
	writeRemote(filepath.Join(root, "other.git"))
	if _, _, err := snapshotOpenStage(ctx, beadsDir, "origin", stage); err == nil {
		t.Fatal("changed persisted authority accepted")
	}
	writeRemote(remoteURL)
	snapshotTestGit(t, filepath.Join(stage, "git"), "config", "--local", "remote.origin.url", filepath.Join(root, "other.git"))
	if _, _, err := snapshotOpenStage(ctx, beadsDir, "origin", stage); err == nil {
		t.Fatal("changed stage Git authority accepted")
	}
	snapshotTestGit(t, filepath.Join(stage, "git"), "config", "--local", "remote.origin.url", remoteURL)
	snapshotTestGit(t, filepath.Join(stage, "git"), "config", "--local", "remote.origin.pushurl", filepath.Join(root, "other.git"))
	if _, _, err := snapshotOpenStage(ctx, beadsDir, "origin", stage); err == nil {
		t.Fatal("changed effective Git push authority accepted")
	}
	if _, _, err := snapshotOpenStage(ctx, filepath.Join(root, "different-beads"), "origin", stage); err == nil {
		t.Fatal("cross-workspace stage substitution accepted")
	}
	for i, secretURL := range []string{
		"https://user:redacted@example.invalid/repo.git",
		"https://example.invalid/repo.git?access_token=redacted",
	} {
		writeRemote(secretURL)
		uncreated := filepath.Join(root, "credential-stage-"+string(rune('0'+i)))
		_, err := CaptureSnapshotBase(ctx, beadsDir, "origin", uncreated)
		if err == nil || strings.Contains(err.Error(), "redacted") {
			t.Fatalf("credential URL did not fail closed without disclosure: %v", err)
		}
		if _, statErr := os.Stat(uncreated); !os.IsNotExist(statErr) {
			t.Fatalf("credential URL created stage before rejection: %v", statErr)
		}
	}
}

// TestSnapshotDoltCommitRoundTrip exercises the handoff from a real local
// post-CAS Dolt commit through frozen NBS files, native Git publication, and
// a fresh NBS reader. All authorities and databases are disposable.
func TestSnapshotDoltCommitRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt CLI unavailable")
	}
	t.Setenv("BEADS_DOLT_DATA_DIR", "")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "NUL")
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "dolt")
	localDir := filepath.Join(dataDir, "snapshot")
	if err := os.MkdirAll(localDir, 0700); err != nil {
		t.Fatal(err)
	}
	runDolt := func(args ...string) {
		t.Helper()
		cmd := exec.Command("dolt", args...) // #nosec G204 -- disposable test database
		cmd.Dir = localDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dolt %v: %v: %s", args, err, out)
		}
	}
	runDolt("init", "--name", "Snapshot Test", "--email", "snapshot@test.invalid")
	// A freshly initialized local repository uses a journal. Clone all
	// reachable R0 chunks into the standalone blobstore before staging Git.
	r0FS, err := filesys.LocalFS.WithWorkingDir(localDir)
	if err != nil {
		t.Fatal(err)
	}
	r0LocalDB, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_DOLT, doltdb.LocalDirDoltDB, r0FS, map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: struct{}{},
	})
	if err != nil {
		t.Fatalf("open local fixture R0: %v", err)
	}
	r0Commit, r0Root, err := snapshotBranchState(ctx, r0LocalDB, ref.NewBranchRef("main"))
	if err != nil {
		t.Fatalf("resolve local fixture R0: %v", err)
	}
	exportDir := filepath.Join(root, "r0-export")
	if err := os.Mkdir(exportDir, 0700); err != nil {
		t.Fatal(err)
	}
	exportBS := blobstore.NewLocalBlobstore(exportDir)
	exportDB, err := snapshotOpenDB(ctx, exportBS, "fixture-export")
	if err != nil {
		t.Fatal(err)
	}
	if err := r0LocalDB.Clone(ctx, root, exportDB, nil); err != nil {
		t.Fatalf("export all reachable R0 NBS sources: %v", err)
	}
	exportCommit, ok := mustResolveSnapshotCommit(ctx, exportDB, hash.Parse(r0Commit))
	if !ok {
		t.Fatal("native R0 export omitted the exact Dolt commit")
	}
	if err := exportDB.SetHeadAndWorkingSetToCommit(ctx, ref.NewBranchRef("main"), exportCommit); err != nil {
		t.Fatal(err)
	}
	exportedCommit, exportedRoot, err := snapshotBranchState(ctx, exportDB, ref.NewBranchRef("main"))
	if err != nil || exportedCommit != r0Commit || exportedRoot != r0Root {
		t.Fatalf("native R0 export differs from local source: %s %s, want %s %s: %v", exportedCommit, exportedRoot, r0Commit, r0Root, err)
	}
	remote := filepath.Join(root, "authority.git")
	if err := os.Mkdir(remote, 0700); err != nil {
		t.Fatal(err)
	}
	snapshotTestGit(t, remote, "init", "--bare")
	snapshotTestGit(t, remote, "config", "--local", "core.hooksPath", filepath.Join(remote, "hooks"))
	writer := filepath.Join(root, "writer")
	if err := os.Mkdir(writer, 0700); err != nil {
		t.Fatal(err)
	}
	snapshotTestGit(t, writer, "init")
	snapshotTestGit(t, writer, "config", "--local", "core.hooksPath", filepath.Join(writer, ".git", "hooks"))
	snapshotTestGit(t, writer, "config", "--local", "user.name", "Snapshot Test")
	snapshotTestGit(t, writer, "config", "--local", "user.email", "snapshot@test.invalid")
	manifest, _, err := blobstore.GetBytes(ctx, exportBS, "manifest", blobstore.AllRange)
	if err != nil {
		t.Fatal(err)
	}
	chunkStore := datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(exportDB))
	tables, ok := chunkStore.(chunks.TableFileStore)
	if !ok {
		t.Fatal("native R0 export has no table file store")
	}
	sources, err := tables.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources.TableFiles) == 0 {
		t.Fatal("native R0 export has no referenced table files")
	}
	if err := os.WriteFile(filepath.Join(writer, "manifest"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	snapshotTestGit(t, writer, "add", "manifest")
	var tableNames []string
	for _, table := range sources.TableFiles {
		name := table.LocationPrefix() + table.FileID() + table.LocationSuffix()
		data, _, err := blobstore.GetBytes(ctx, exportBS, name, blobstore.AllRange)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(writer, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(writer, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		snapshotTestGit(t, writer, "add", name)
		tableNames = append(tableNames, name)
	}
	snapshotTestGit(t, writer, "commit", "-m", "R0")
	snapshotTestGit(t, writer, "push", remote, "HEAD:refs/dolt/data")
	statePath := filepath.Join(localDir, ".dolt", "repo_state.json")
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatal(err)
	}
	state["remotes"] = map[string]any{"origin": map[string]string{"url": filepath.ToSlash(remote)}}
	stateBytes, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, stateBytes, 0600); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(root, ".beads")
	if err := os.Mkdir(beadsDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := configfile.DefaultConfig()
	cfg.DoltDatabase = "snapshot"
	cfg.DoltDataDir = filepath.Join("..", "dolt")
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	base, err := CaptureSnapshotBase(ctx, beadsDir, "origin", stage)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disposable captured R0 tree: %s", snapshotTestGit(t, filepath.Join(stage, "git"), "ls-tree", "--name-only", base.Base.R0GitOID))
	_, capturedStore, err := snapshotOpenStage(ctx, beadsDir, "origin", stage)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append([]string{"manifest"}, tableNames...) {
		want, _, err := blobstore.GetBytes(ctx, exportBS, name, blobstore.AllRange)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := blobstore.GetBytes(ctx, capturedStore, name, blobstore.AllRange)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("captured R0 blob %s differs from local fixture: got=%d want=%d err=%v", name, len(got), len(want), err)
		}
	}
	capturedDB, err := snapshotOpenDB(ctx, capturedStore, "fixture-captured-r0")
	if err != nil {
		t.Fatalf("open captured R0 through no-conjoin NBS: %v", err)
	}
	capturedCommit, _, err := snapshotBranchState(ctx, capturedDB, ref.NewBranchRef("main"))
	if err != nil || capturedCommit != r0Commit {
		t.Fatalf("captured R0 commit = %s, local fixture R0 = %s: %v", capturedCommit, r0Commit, err)
	}
	if err := capturedDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := exportDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r0LocalDB.Close(); err != nil {
		t.Fatal(err)
	}
	if base.Base.Ref != snapshotRef || base.Base.Authority == "" {
		t.Fatalf("native base omitted canonical binding: %+v", base.Base)
	}
	runDolt("sql", "-q", "CREATE TABLE sample (id int PRIMARY KEY, value text)")
	runDolt("sql", "-q", "INSERT INTO sample VALUES (1, 'roundtrip')")
	runDolt("add", ".")
	runDolt("commit", "-m", "post-CAS")
	localFS, err := filesys.LocalFS.WithWorkingDir(localDir)
	if err != nil {
		t.Fatal(err)
	}
	localDB, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_DOLT, doltdb.LocalDirDoltDB, localFS, map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: struct{}{},
	})
	if err != nil {
		t.Fatal(err)
	}
	localCommit, localRoot, err := snapshotBranchState(ctx, localDB, ref.NewBranchRef("main"))
	// The Dolt CLI and direct API may share a journal writer in this fixture;
	// the CLI can already have closed it. The observed commit/root above are the
	// acceptance evidence, and cleanup still attempts to release this handle.
	_ = localDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareSnapshot(ctx, beadsDir, "origin", stage, localCommit, localRoot, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Candidate.PublicationGitOID == "" {
		t.Fatal("native preparation omitted retained candidate")
	}
	for _, recoverOnly := range []bool{false, true} {
		if _, err := FinishSnapshot(ctx, beadsDir, "other", stage, recoverOnly); err == nil {
			t.Fatalf("publication operation accepted different selected remote (recover=%t)", recoverOnly)
		}
	}
	if _, err := os.Stat(filepath.Join(stage, "publish.started")); !os.IsNotExist(err) {
		t.Fatalf("remote mismatch started publication: %v", err)
	}
	// Simulate losing candidate.json after native preparation acknowledged its
	// local lookup. Recovery must recover that exact retained object, without
	// re-freezing the source or producing a replacement candidate.
	if err := os.Remove(filepath.Join(stage, "candidate.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "candidate.json.tmp"), []byte("orphaned crash temp"), 0600); err != nil {
		t.Fatal(err)
	}
	if binary := os.Getenv("BEADS_SNAPSHOT_TEST_BD_BINARY"); binary != "" {
		cmd := exec.Command(binary, "dolt", "push", "snapshot", "recover", "--stage-dir", stage, "--remote", "origin") // #nosec G204 -- explicitly selected private test binary
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("private bd snapshot recover: %v: %s", err, output)
		}
		var cliOutcome blobstore.SnapshotOutcome
		if err := json.Unmarshal(output, &cliOutcome); err != nil {
			t.Fatalf("private bd snapshot recover output: %v: %s", err, output)
		}
		if cliOutcome.Status != blobstore.SnapshotPrepared || cliOutcome.Candidate.PublicationGitOID != prepared.Candidate.PublicationGitOID {
			t.Fatalf("private bd snapshot recover changed retained candidate: %+v", cliOutcome)
		}
	}
	lookupOutcome, err := FinishSnapshot(ctx, beadsDir, "origin", stage, true)
	if err != nil || lookupOutcome.Status != blobstore.SnapshotPrepared || lookupOutcome.Candidate.PublicationGitOID != prepared.Candidate.PublicationGitOID {
		t.Fatalf("retained candidate lookup: %+v, %v", lookupOutcome, err)
	}
	outcome, err := FinishSnapshot(ctx, beadsDir, "origin", stage, false)
	if err != nil || outcome.Status != blobstore.SnapshotConfirmed {
		t.Fatalf("publication: %+v, %v", outcome, err)
	}
	recoverOutcome, err := FinishSnapshot(ctx, beadsDir, "origin", stage, true)
	if err != nil || recoverOutcome.Status != blobstore.SnapshotConfirmed {
		t.Fatalf("recovery: %+v, %v", recoverOutcome, err)
	}
	readerDir := filepath.Join(root, "reader.git")
	if err := os.Mkdir(readerDir, 0700); err != nil {
		t.Fatal(err)
	}
	snapshotTestGit(t, readerDir, "init", "--bare")
	snapshotTestGit(t, readerDir, "remote", "add", "origin", filepath.ToSlash(remote))
	reader, err := blobstore.NewGitBlobstoreWithOptions(readerDir, snapshotRef, blobstore.GitBlobstoreOptions{RemoteName: "origin"})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := snapshotOpenDB(ctx, reader, "snapshot-reopened")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	gotCommit, gotRoot, err := snapshotBranchState(ctx, reopened, ref.NewBranchRef("main"))
	if err != nil || gotCommit != localCommit || gotRoot != localRoot {
		t.Fatalf("fresh NBS read differs: commit %s root %s, want %s %s: %v", gotCommit, gotRoot, localCommit, localRoot, err)
	}
}
