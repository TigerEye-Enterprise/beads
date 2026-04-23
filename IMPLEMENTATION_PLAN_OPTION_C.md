# Option C — Config-Driven ID Pattern for bd CLI

**Branch**: `feat/config-driven-id-pattern` (this branch)
**Upstream PR target**: `steveyegge/beads:main`
**TigerEye bead**: TE-z7b6p
**CEO authorized**: 2026-04-23 per SO-NO-UPSTREAM-CONTRIBUTION-001 exception

## Problem

`bd create` rejects human-readable semantic IDs like `TE-nvidia-nim-verification-2026-04-17` because `internal/utils/issue_id.go:isLikelyHash()` hardcodes a 3-8 char alphanumeric suffix constraint. Agents using semantic names for dispatch-log traceability cannot adopt them.

## Root cause

`internal/utils/issue_id.go`:
```go
func isLikelyHash(s string) bool {
    if len(s) < 3 || len(s) > 8 { return false }  // hardcoded
    // 4+ char suffixes require at least one digit
    // base36 only
}
```

Any suffix outside 3-8 chars OR without a digit fails prefix extraction, which cascades to `ValidateIDFormat` rejection.

## Option C solution — config-driven, backward-compatible

Add optional `id-pattern` field to `.beads/config.yaml`:

```yaml
issue-prefix: "TE"

# Optional: override default ID validation regex.
# When unset (default), preserves current isLikelyHash behavior (3-8 char base36
# suffix with digit). When set, uses provided regex as authoritative validator.
# Pattern matches the FULL ID including prefix.
id-pattern: "^TE-[a-z0-9-]{4,60}$"
```

## Implementation

1. **`internal/config/config.go`**: Add `IDPattern string \`yaml:"id-pattern"\`` field to Config struct.
2. **`internal/utils/issue_id.go`**: Add new `ValidateIDWithPattern(id, pattern string)` helper. Call from `ValidateIDFormat` when pattern is configured.
3. **`internal/validation/bead.go`**: Plumb pattern from config into `ValidateIDFormat` callsite.
4. **`internal/utils/issue_id_test.go`**: Add test cases for config-driven pattern (short prefix, long semantic suffix, hyphenated suffix, regex compile error fallback).
5. **`docs/config-reference.md`** (if exists) OR **`README.md`**: Document `id-pattern` field with examples.
6. **Backward compat**: All existing tests MUST pass unchanged. Default behavior preserved when `id-pattern` unset.

## Test matrix

| Config | Input | Expected |
|---|---|---|
| no id-pattern | `TE-abc123` | VALID (current behavior) |
| no id-pattern | `TE-nvidia-nim-verification-2026` | INVALID (current behavior) |
| `id-pattern: "^TE-[a-z0-9-]{4,60}$"` | `TE-nvidia-nim-verification` | VALID |
| `id-pattern: "^TE-[a-z0-9-]{4,60}$"` | `TE-x` (too short) | INVALID |
| `id-pattern: "^.*$"` (permissive) | anything | VALID |
| `id-pattern: "[invalid-regex"` | anything | config-load error at startup |

## Out of scope

- Migration of existing IDs (idempotent — existing IDs remain valid under any permissive pattern)
- Changing default regex (preserves current behavior)
- Multi-pattern support (single pattern suffices for vast majority of use cases)

## PR description template

> Adds optional `id-pattern` config field for user-specified ID regex. Preserves existing `isLikelyHash` behavior as default. Enables semantic ID naming (e.g., `TE-auth-migration-phase3`) for teams prioritizing human-readable dispatch-log traceability over hash-based uniqueness. No breaking changes — opt-in per `.beads/config.yaml`.
>
> Fixes: #<upstream-issue-TBD>  (file issue before PR per contribution flow)
