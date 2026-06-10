# run_all.ps1 — 全シナリオを順番に実行し PASS/FAIL 一覧を表示する。
# 事前に patrol-sim.exe / fake-adb.exe をビルドする（ビルド込み）。
# 1つでも FAIL なら exit 1 を返す（CI 利用可能）。

param(
    [string]$ScenariosDir = "scenarios",
    [string]$LogDir = "logs\sim",
    [switch]$Verbose
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ── ビルド ──────────────────────────────────────────────────────────────
Write-Host "=== ビルド ===" -ForegroundColor Cyan

Write-Host "  go build -o release\patrol-sim.exe ./cmd/patrol-sim" -ForegroundColor DarkGray
& go build -o release\patrol-sim.exe ./cmd/patrol-sim
if ($LASTEXITCODE -ne 0) {
    Write-Host "FAIL: patrol-sim.exe ビルド失敗" -ForegroundColor Red
    exit 1
}

Write-Host "  go build -o release\fake-adb.exe ./cmd/fake-adb" -ForegroundColor DarkGray
& go build -o release\fake-adb.exe ./cmd/fake-adb
if ($LASTEXITCODE -ne 0) {
    Write-Host "FAIL: fake-adb.exe ビルド失敗" -ForegroundColor Red
    exit 1
}

Write-Host "  ビルド完了" -ForegroundColor Green

# ── ログディレクトリ作成 ─────────────────────────────────────────────────
if (-not (Test-Path $LogDir)) {
    New-Item -ItemType Directory -Path $LogDir -Force | Out-Null
}

# ── シナリオ列挙 ─────────────────────────────────────────────────────────
$scenarios = Get-ChildItem -Path $ScenariosDir -Filter "*.json" | Sort-Object Name
if ($scenarios.Count -eq 0) {
    Write-Host "FAIL: $ScenariosDir にシナリオが見つかりません" -ForegroundColor Red
    exit 1
}

Write-Host ""
Write-Host "=== シナリオ実行 ($($scenarios.Count) 件) ===" -ForegroundColor Cyan

$results = @()
$totalStart = Get-Date

foreach ($scenario in $scenarios) {
    $name = $scenario.BaseName
    $logFile = Join-Path $LogDir "$name.log"
    $start = Get-Date

    Write-Host "  [$name] 実行中..." -NoNewline

    $scenarioPath = $scenario.FullName
    $simExe = Join-Path $PSScriptRoot "release\patrol-sim.exe"

    # & 演算子で直接起動し、stdout+stderr を両方キャプチャしてログファイルへ保存
    if ($Verbose) {
        & $simExe -scenario $scenarioPath -v 2>&1 | Tee-Object -FilePath $logFile | Out-Null
    } else {
        & $simExe -scenario $scenarioPath 2>&1 | Tee-Object -FilePath $logFile | Out-Null
    }
    $exitCode = $LASTEXITCODE

    $elapsed = (Get-Date) - $start
    $passed = ($exitCode -eq 0)
    $status = if ($passed) { "PASS" } else { "FAIL" }
    $color  = if ($passed) { "Green" } else { "Red" }

    Write-Host " $status  ($([math]::Round($elapsed.TotalSeconds, 1))s)" -ForegroundColor $color

    $results += [PSCustomObject]@{
        Name    = $name
        Pass    = $passed
        Elapsed = $elapsed
        LogFile = $logFile
    }
}

$totalElapsed = (Get-Date) - $totalStart

# ── 結果一覧 ─────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "=== 結果一覧 ===" -ForegroundColor Cyan
$passCount = 0
$failCount = 0
foreach ($r in $results) {
    $status = if ($r.Pass) { "PASS" } else { "FAIL" }
    $color  = if ($r.Pass) { "Green" } else { "Red" }
    $secs   = [math]::Round($r.Elapsed.TotalSeconds, 1)
    Write-Host ("  {0,-6} {1,-40} ({2}s)" -f $status, $r.Name, $secs) -ForegroundColor $color
    if ($r.Pass) { $passCount++ } else { $failCount++ }
}

Write-Host ""
Write-Host ("合計: {0} PASS / {1} FAIL  (所要時間: {2}s)" -f `
    $passCount, $failCount, [math]::Round($totalElapsed.TotalSeconds, 1))

if ($failCount -gt 0) {
    Write-Host ""
    Write-Host "FAIL したシナリオのログ:" -ForegroundColor Red
    foreach ($r in $results | Where-Object { -not $r.Pass }) {
        Write-Host "  $($r.LogFile)" -ForegroundColor Yellow
    }
    exit 1
}

exit 0
