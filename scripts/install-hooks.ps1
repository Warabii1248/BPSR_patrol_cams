# BPSR_patrol_cams git hooks installer
# Usage: .\scripts\install-hooks.ps1  (run from anywhere inside the repo)

$ErrorActionPreference = 'Stop'

# Ask git for the repository root (based on cwd)
$repoRoot = & git rev-parse --show-toplevel 2>$null
if (-not $repoRoot) {
    Write-Error "Not inside a git repository. cd into the repo first."
    exit 1
}
$repoRoot = $repoRoot.Trim()
Write-Host "repoRoot = $repoRoot"

# Set core.hooksPath (path relative to the repo root)
& git -C $repoRoot config core.hooksPath scripts/git-hooks
$current = & git -C $repoRoot config --get core.hooksPath
Write-Host "core.hooksPath = $current"

$hookPath = Join-Path $repoRoot "scripts\git-hooks\pre-commit"
if (Test-Path $hookPath) {
    Write-Host "pre-commit hook found: scripts/git-hooks/pre-commit"
    Write-Host "From now on, 'git commit' will run: go vet / go build / go test (-race if gcc)."
} else {
    Write-Warning "$hookPath not found."
}
