package dolt

// This file deliberately keeps snapshot publication separate from the legacy
// Dolt push path. A snapshot is a retained, fixed-R0 operation, not a sync.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/blobstore"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/storage/doltutil"
)

const snapshotRef = "refs/dolt/data"
const snapshotBaseRef = "refs/dolt/snapshot-base"
const snapshotMemTableSize = 128 << 20

var snapshotRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var snapshotDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SnapshotStageBase binds a private stage to the selected local Dolt database
// and its persisted Git authority. Paths are absolute to reject stage reuse
// from a different workspace.
type SnapshotStageBase struct {
	StageDir     string                 `json:"stage_dir"`
	BeadsDir     string                 `json:"beads_dir"`
	LocalDir     string                 `json:"local_dir"`
	AuthorityURL string                 `json:"authority_url"`
	AuthorityRef string                 `json:"authority_ref"`
	Base         blobstore.SnapshotBase `json:"base"`
}

type SnapshotStageCandidate struct {
	Base        SnapshotStageBase           `json:"base"`
	LocalCommit string                      `json:"local_commit"`
	LocalRoot   string                      `json:"local_root"`
	Candidate   blobstore.SnapshotCandidate `json:"candidate"`
}

type snapshotStageRequest struct {
	Base        SnapshotStageBase         `json:"base"`
	LocalCommit string                    `json:"local_commit"`
	LocalRoot   string                    `json:"local_root"`
	Request     blobstore.SnapshotRequest `json:"request"`
}

func snapshotGit(ctx context.Context, dir string, args ...string) (string, error) {
	argv := append([]string{"--git-dir", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...) // #nosec G204 -- fixed executable and argv, no shell
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git snapshot stage: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func snapshotSelectedLocal(beadsDir, remote string) (string, string, error) {
	if !snapshotRemoteName.MatchString(remote) {
		return "", "", fmt.Errorf("snapshot: invalid configured remote name")
	}
	cfg, err := configfile.LoadForDiscovery(beadsDir)
	if err != nil || cfg == nil {
		return "", "", fmt.Errorf("snapshot: selected workspace configuration unavailable: %w", err)
	}
	dbName := cfg.GetDoltDatabase()
	if dbName == "" || dbName == "." || dbName == ".." || filepath.Base(dbName) != dbName {
		return "", "", fmt.Errorf("snapshot: invalid selected Dolt database name")
	}
	localDir, err := filepath.Abs(filepath.Join(doltserver.ResolveDoltDir(beadsDir), dbName))
	if err != nil {
		return "", "", err
	}
	remotes, err := doltutil.PersistedRemotes(localDir)
	if err != nil {
		return "", "", err
	}
	for _, r := range remotes {
		if r.Name == remote && r.URL != "" {
			parsed, err := url.Parse(r.URL)
			if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
				return "", "", fmt.Errorf("snapshot: authority URL must not embed credentials, query, or fragment")
			}
			return localDir, r.URL, nil
		}
	}
	return "", "", fmt.Errorf("snapshot: remote %q is not persisted in selected Dolt database", remote)
}

func snapshotWriteExclusive(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("snapshot: retained receipt already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// A same-directory hard link publishes the fully synced receipt without
	// replacing an existing one. A crash can leave an unrelated temporary file,
	// but a later original-candidate lookup can still write its own receipt.
	return os.Link(f.Name(), path)
}

func snapshotRead(path string, value any) error {
	data, err := os.ReadFile(path) // #nosec G304 -- stage path selected by operator
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("snapshot: invalid retained receipt %s: %w", path, err)
	}
	return nil
}

func snapshotValidateGitAuthority(ctx context.Context, gitDir, remote, expectedURL string) error {
	configuredURL, err := snapshotGit(ctx, gitDir, "config", "--local", "--get", "remote."+remote+".url")
	if err != nil || configuredURL != expectedURL {
		return fmt.Errorf("snapshot: stage Git authority mapping changed: %w", err)
	}
	for _, args := range [][]string{{"remote", "get-url", remote}, {"remote", "get-url", "--push", remote}} {
		effectiveURL, err := snapshotGit(ctx, gitDir, args...)
		if err != nil || effectiveURL != expectedURL {
			return fmt.Errorf("snapshot: effective Git authority changed: %w", err)
		}
	}
	return nil
}

func snapshotOpenStage(ctx context.Context, beadsDir, remote, stageDir string) (SnapshotStageBase, *blobstore.GitBlobstore, error) {
	beadsDir, err := filepath.Abs(beadsDir)
	if err != nil {
		return SnapshotStageBase{}, nil, err
	}
	stageDir, err = filepath.Abs(stageDir)
	if err != nil {
		return SnapshotStageBase{}, nil, err
	}
	var saved SnapshotStageBase
	if err := snapshotRead(filepath.Join(stageDir, "base.json"), &saved); err != nil {
		return saved, nil, err
	}
	if saved.StageDir != stageDir || saved.BeadsDir != beadsDir || saved.AuthorityRef != snapshotRef || saved.Base.Remote != remote || saved.beadsDirInvalid() {
		return saved, nil, fmt.Errorf("snapshot: stage identity or authority ref differs")
	}
	localDir, authorityURL, err := snapshotSelectedLocal(saved.BeadsDir, saved.Base.Remote)
	if err != nil {
		return saved, nil, err
	}
	if localDir != saved.LocalDir || authorityURL != saved.AuthorityURL {
		return saved, nil, fmt.Errorf("snapshot: selected local database or persisted authority changed")
	}
	gitDir := filepath.Join(stageDir, "git")
	if err := snapshotValidateGitAuthority(ctx, gitDir, saved.Base.Remote, saved.AuthorityURL); err != nil {
		return saved, nil, err
	}
	retainedR0, err := snapshotGit(ctx, gitDir, "rev-parse", "--verify", snapshotBaseRef+"^{commit}")
	if err != nil || retainedR0 != saved.Base.R0GitOID {
		return saved, nil, fmt.Errorf("snapshot: retained R0 differs from base receipt: %w", err)
	}
	gbs, err := blobstore.NewGitBlobstoreWithOptions(gitDir, snapshotRef, blobstore.GitBlobstoreOptions{RemoteName: saved.Base.Remote})
	return saved, gbs, err
}

func (s SnapshotStageBase) beadsDirInvalid() bool {
	return s.BeadsDir == "" || s.LocalDir == "" || s.AuthorityURL == ""
}

// CaptureSnapshotBase creates a new private stage and records the canonical R0
// before semantic CAS. The stage path must not exist; a failed initialization
// remains inspectable and cannot silently replace another request.
func CaptureSnapshotBase(ctx context.Context, beadsDir, remote, stageDir string) (SnapshotStageBase, error) {
	beadsDir, err := filepath.Abs(beadsDir)
	if err != nil {
		return SnapshotStageBase{}, err
	}
	stageDir, err = filepath.Abs(stageDir)
	if err != nil {
		return SnapshotStageBase{}, err
	}
	localDir, authorityURL, err := snapshotSelectedLocal(beadsDir, remote)
	if err != nil {
		return SnapshotStageBase{}, err
	}
	if err := os.Mkdir(stageDir, 0700); err != nil {
		return SnapshotStageBase{}, fmt.Errorf("snapshot: stage directory must be new: %w", err)
	}
	gitDir := filepath.Join(stageDir, "git")
	if err := os.Mkdir(gitDir, 0700); err != nil {
		return SnapshotStageBase{}, err
	}
	if _, err := snapshotGit(ctx, gitDir, "init", "--bare"); err != nil {
		return SnapshotStageBase{}, err
	}
	if _, err := snapshotGit(ctx, gitDir, "remote", "add", remote, authorityURL); err != nil {
		return SnapshotStageBase{}, err
	}
	if err := snapshotValidateGitAuthority(ctx, gitDir, remote, authorityURL); err != nil {
		return SnapshotStageBase{}, err
	}
	gbs, err := blobstore.NewGitBlobstoreWithOptions(gitDir, snapshotRef, blobstore.GitBlobstoreOptions{RemoteName: remote})
	if err != nil {
		return SnapshotStageBase{}, err
	}
	base, err := gbs.ReadSnapshotBase(ctx)
	if err != nil {
		return SnapshotStageBase{}, err
	}
	if _, err := snapshotGit(ctx, gitDir, "update-ref", snapshotBaseRef, base.R0GitOID); err != nil {
		return SnapshotStageBase{}, err
	}
	saved := SnapshotStageBase{StageDir: stageDir, BeadsDir: beadsDir, LocalDir: localDir, AuthorityURL: authorityURL, AuthorityRef: snapshotRef, Base: base}
	if err := snapshotWriteExclusive(filepath.Join(stageDir, "base.json"), saved); err != nil {
		return SnapshotStageBase{}, err
	}
	return saved, nil
}

func snapshotOpenDB(ctx context.Context, bs blobstore.Blobstore, name string) (*doltdb.DoltDB, error) {
	cs, err := nbs.NewNoConjoinBSStore(ctx, types.Format_DOLT.VersionString(), bs, snapshotMemTableSize, nbs.NewUnlimitedMemQuotaProvider())
	if err != nil {
		return nil, err
	}
	return doltdb.DoltDBFromCS(cs, name)
}

// snapshotPinnedBaselineStore exposes only the saved immutable R0 to NBS.
// GitBlobstore's read path fetches a remote on every new instance, so reading
// the authority directly after capture would admit a later canonical tree.
func snapshotPinnedBaselineStore(ctx context.Context, saved SnapshotStageBase) (*blobstore.GitBlobstore, error) {
	baselineGitDir := filepath.Join(saved.StageDir, "baseline.git")
	if err := os.Mkdir(baselineGitDir, 0700); err != nil {
		return nil, err
	}
	if _, err := snapshotGit(ctx, baselineGitDir, "init", "--bare"); err != nil {
		return nil, err
	}
	if _, err := snapshotGit(ctx, baselineGitDir, "fetch", "--no-tags", filepath.Join(saved.StageDir, "git"), snapshotBaseRef); err != nil {
		return nil, err
	}
	fetchedR0, err := snapshotGit(ctx, baselineGitDir, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil || fetchedR0 != saved.Base.R0GitOID {
		return nil, fmt.Errorf("snapshot: pinned baseline differs from saved R0: %w", err)
	}
	if _, err := snapshotGit(ctx, baselineGitDir, "update-ref", snapshotRef, saved.Base.R0GitOID); err != nil {
		return nil, err
	}
	baselineReaderDir := filepath.Join(saved.StageDir, "baseline-reader.git")
	if err := os.Mkdir(baselineReaderDir, 0700); err != nil {
		return nil, err
	}
	if _, err := snapshotGit(ctx, baselineReaderDir, "init", "--bare"); err != nil {
		return nil, err
	}
	if _, err := snapshotGit(ctx, baselineReaderDir, "remote", "add", "origin", baselineGitDir); err != nil {
		return nil, err
	}
	return blobstore.NewGitBlobstoreWithOptions(baselineReaderDir, snapshotRef, blobstore.GitBlobstoreOptions{RemoteName: "origin"})
}

func snapshotBranchState(ctx context.Context, db *doltdb.DoltDB, branch ref.BranchRef) (string, string, error) {
	commit, err := db.ResolveCommitRef(ctx, branch)
	if err != nil {
		return "", "", err
	}
	h, err := commit.HashOf()
	if err != nil {
		return "", "", err
	}
	roots, err := db.ResolveBranchRoots(ctx, branch)
	if err != nil {
		return "", "", err
	}
	headRoot, err := roots.Head.HashOf()
	if err != nil {
		return "", "", err
	}
	workingRoot, err := roots.Working.HashOf()
	if err != nil {
		return "", "", err
	}
	stagedRoot, err := roots.Staged.HashOf()
	if err != nil {
		return "", "", err
	}
	if headRoot != workingRoot || headRoot != stagedRoot {
		return "", "", fmt.Errorf("snapshot: local branch has uncommitted working or staged state")
	}
	return h.String(), headRoot.String(), nil
}

// PrepareSnapshot freezes the saved R0 plus exactly one selected local Dolt
// commit into a private local blobstore before any native preparation call.
func PrepareSnapshot(ctx context.Context, beadsDir, remote, stageDir, localCommit, localRoot, intentSHA256 string) (SnapshotStageCandidate, error) {
	saved, gbs, err := snapshotOpenStage(ctx, beadsDir, remote, stageDir)
	if err != nil {
		return SnapshotStageCandidate{}, err
	}
	if !hash.IsValid(localCommit) || !hash.IsValid(localRoot) || !snapshotDigest.MatchString(intentSHA256) {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: exact local commit/root and lowercase SHA256 intent required")
	}
	currentBase, err := gbs.ReadSnapshotBase(ctx)
	if err != nil || currentBase != saved.Base {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: saved canonical R0 is stale before preparation: %w", err)
	}
	marker, err := os.OpenFile(filepath.Join(saved.StageDir, "prepare.started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: preparation already started; inspect retained stage: %w", err)
	}
	if err := marker.Close(); err != nil {
		return SnapshotStageCandidate{}, err
	}
	branch := ref.NewBranchRef("main")
	baselineStore, err := snapshotPinnedBaselineStore(ctx, saved)
	if err != nil {
		return SnapshotStageCandidate{}, err
	}
	baseDB, err := snapshotOpenDB(ctx, baselineStore, "snapshot-base")
	if err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: open pinned R0 NBS: %w", err)
	}
	defer func() { _ = baseDB.Close() }()
	baseCommit, err := baseDB.ResolveCommitRef(ctx, branch)
	if err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: resolve pinned R0 Dolt commit: %w", err)
	}
	baseHash, err := baseCommit.HashOf()
	if err != nil {
		return SnapshotStageCandidate{}, err
	}
	localFS, err := filesys.LocalFS.WithWorkingDir(saved.LocalDir)
	if err != nil {
		return SnapshotStageCandidate{}, err
	}
	localDB, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_DOLT, doltdb.LocalDirDoltDB, localFS, map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: struct{}{},
	})
	if err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: open selected local Dolt database: %w", err)
	}
	defer func() { _ = localDB.Close() }()
	observedCommit, observedRoot, err := snapshotBranchState(ctx, localDB, branch)
	if err != nil || observedCommit != localCommit || observedRoot != localRoot {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: local branch differs from recorded commit/root: %w", err)
	}
	localHead, err := localDB.ResolveCommitRef(ctx, branch)
	if err != nil {
		return SnapshotStageCandidate{}, err
	}
	if localHead.NumParents() != 1 {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: local commit must have exactly one parent")
	}
	parent, err := localHead.GetParent(ctx, 0)
	if err != nil || parent.Addr != baseHash {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: local commit is not a direct child of canonical R0 Dolt commit: %w", err)
	}
	localBlobDir := filepath.Join(saved.StageDir, "frozen")
	if err := os.Mkdir(localBlobDir, 0700); err != nil {
		return SnapshotStageCandidate{}, err
	}
	frozenBS := blobstore.NewLocalBlobstore(localBlobDir)
	frozenDB, err := snapshotOpenDB(ctx, frozenBS, "snapshot-frozen")
	if err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: open frozen NBS: %w", err)
	}
	defer func() { _ = frozenDB.Close() }()
	if err := baseDB.Clone(ctx, saved.StageDir, frozenDB, nil); err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: clone pinned R0 into frozen store: %w", err)
	}
	if err := frozenDB.PullChunks(ctx, saved.StageDir, localDB, []hash.Hash{hash.Parse(localCommit)}, nil, nil); err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: pull exact local commit into frozen store: %w", err)
	}
	frozenCommit, ok := mustResolveSnapshotCommit(ctx, frozenDB, hash.Parse(localCommit))
	if !ok {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: frozen exact commit unavailable")
	}
	if err := frozenDB.SetHeadAndWorkingSetToCommit(ctx, branch, frozenCommit); err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: set frozen branch to exact commit: %w", err)
	}
	frozenHash, frozenRoot, err := snapshotBranchState(ctx, frozenDB, branch)
	if err != nil || frozenHash != localCommit || frozenRoot != observedRoot {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: frozen commit/root differs: %w", err)
	}
	afterCommit, afterRoot, err := snapshotBranchState(ctx, localDB, branch)
	if err != nil || afterCommit != localCommit || afterRoot != observedRoot {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: local source changed during freeze: %w", err)
	}
	baseAgain, err := gbs.ReadSnapshotBase(ctx)
	if err != nil || baseAgain != saved.Base {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: saved canonical R0 changed during freeze: %w", err)
	}
	manifest, _, err := blobstore.GetBytes(ctx, frozenBS, "manifest", blobstore.AllRange)
	if err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: read frozen NBS manifest: %w", err)
	}
	cs := datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(frozenDB))
	files, ok := cs.(chunks.TableFileStore)
	if !ok {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: frozen chunk store has no table files")
	}
	sources, err := files.Sources(ctx)
	if err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: list frozen NBS table files: %w", err)
	}
	if len(sources.TableFiles) == 0 {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: frozen manifest has no table files")
	}
	for _, table := range sources.TableFiles {
		key := table.LocationPrefix() + table.FileID() + table.LocationSuffix()
		reader, size, err := table.Open(ctx)
		if err != nil {
			return SnapshotStageCandidate{}, fmt.Errorf("snapshot: open frozen NBS table: %w", err)
		}
		_, putErr := gbs.Put(ctx, key, int64(size), reader)
		if err := errors.Join(putErr, reader.Close()); err != nil {
			return SnapshotStageCandidate{}, fmt.Errorf("snapshot: stage frozen NBS table %s: %w", key, err)
		}
	}
	preview := blobstore.SnapshotPreview{Remote: saved.Base.Remote, Ref: saved.Base.Ref, Authority: saved.Base.Authority, R0GitOID: saved.Base.R0GitOID, ExpectedManifestVersion: saved.Base.ManifestVersion}
	digest, err := gbs.PreviewSnapshotSHA256(ctx, preview, manifest)
	if err != nil {
		return SnapshotStageCandidate{}, fmt.Errorf("snapshot: preview frozen NBS content: %w", err)
	}
	req := blobstore.SnapshotRequest{Remote: preview.Remote, Ref: preview.Ref, Authority: preview.Authority, R0GitOID: preview.R0GitOID, ExpectedManifestVersion: preview.ExpectedManifestVersion, SnapshotSHA256: digest, IntentSHA256: intentSHA256}
	if err := snapshotWriteExclusive(filepath.Join(saved.StageDir, "request.json"), snapshotStageRequest{
		Base: saved, LocalCommit: localCommit, LocalRoot: observedRoot, Request: req,
	}); err != nil {
		return SnapshotStageCandidate{}, err
	}
	candidate, prepareErr := gbs.PrepareSnapshot(ctx, req, manifest)
	result := SnapshotStageCandidate{Base: saved, LocalCommit: localCommit, LocalRoot: observedRoot, Candidate: candidate}
	if candidate.PublicationGitOID != "" {
		if err := snapshotWriteExclusive(filepath.Join(saved.StageDir, "candidate.json"), result); err != nil {
			return result, errors.Join(prepareErr, err)
		}
	}
	if prepareErr != nil {
		return result, fmt.Errorf("snapshot: native preparation: %w", prepareErr)
	}
	return result, nil
}

func mustResolveSnapshotCommit(ctx context.Context, db *doltdb.DoltDB, h hash.Hash) (*doltdb.Commit, bool) {
	optional, err := db.ResolveHash(ctx, h)
	if err != nil || optional == nil {
		return nil, false
	}
	return optional.ToCommit()
}

// FinishSnapshot uses only the retained candidate and validates the authority
// binding again before the sole native publish or recovery attempt.
func FinishSnapshot(ctx context.Context, beadsDir, remote, stageDir string, recoverOnly bool) (blobstore.SnapshotOutcome, error) {
	saved, gbs, err := snapshotOpenStage(ctx, beadsDir, remote, stageDir)
	if err != nil {
		return blobstore.SnapshotOutcome{}, err
	}
	var requested snapshotStageRequest
	if err := snapshotRead(filepath.Join(saved.StageDir, "request.json"), &requested); err != nil {
		return blobstore.SnapshotOutcome{}, err
	}
	if requested.Base != saved || requested.Request.Remote != saved.Base.Remote || requested.Request.Ref != saved.Base.Ref || requested.Request.Authority != saved.Base.Authority || requested.Request.R0GitOID != saved.Base.R0GitOID || requested.Request.ExpectedManifestVersion != saved.Base.ManifestVersion {
		return blobstore.SnapshotOutcome{}, fmt.Errorf("snapshot: retained request differs from saved base")
	}
	var receipt SnapshotStageCandidate
	if err := snapshotRead(filepath.Join(saved.StageDir, "candidate.json"), &receipt); err != nil {
		if !recoverOnly || !errors.Is(err, os.ErrNotExist) {
			return blobstore.SnapshotOutcome{}, fmt.Errorf("snapshot: candidate receipt unavailable: %w", err)
		}
		candidate, lookupErr := gbs.LookupSnapshot(ctx, requested.Request)
		if lookupErr != nil {
			return blobstore.SnapshotOutcome{}, lookupErr
		}
		if candidate.SnapshotRequest != requested.Request || candidate.PublicationGitOID == "" {
			return blobstore.SnapshotOutcome{}, fmt.Errorf("snapshot: recovered candidate differs from retained request")
		}
		receipt = SnapshotStageCandidate{Base: saved, LocalCommit: requested.LocalCommit, LocalRoot: requested.LocalRoot, Candidate: candidate}
		if err := snapshotWriteExclusive(filepath.Join(saved.StageDir, "candidate.json"), receipt); err != nil {
			return blobstore.SnapshotOutcome{}, err
		}
	}
	if requested.Base != saved || requested.LocalCommit != receipt.LocalCommit || requested.LocalRoot != receipt.LocalRoot || requested.Request != receipt.Candidate.SnapshotRequest {
		return blobstore.SnapshotOutcome{}, fmt.Errorf("snapshot: candidate differs from retained request")
	}
	if receipt.Base != saved || receipt.Candidate.Remote != saved.Base.Remote || receipt.Candidate.Ref != saved.Base.Ref || receipt.Candidate.Authority != saved.Base.Authority || receipt.Candidate.R0GitOID != saved.Base.R0GitOID || receipt.Candidate.ExpectedManifestVersion != saved.Base.ManifestVersion || receipt.Candidate.PublicationGitOID == "" {
		return blobstore.SnapshotOutcome{}, fmt.Errorf("snapshot: candidate receipt differs from saved base")
	}
	if recoverOnly {
		return gbs.RecoverSnapshot(ctx, receipt.Candidate), nil
	}
	marker, err := os.OpenFile(filepath.Join(saved.StageDir, "publish.started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return blobstore.SnapshotOutcome{}, fmt.Errorf("snapshot: publish already attempted; use recover: %w", err)
	}
	if err := marker.Close(); err != nil {
		return blobstore.SnapshotOutcome{}, err
	}
	return gbs.PublishSnapshot(ctx, receipt.Candidate), nil
}
