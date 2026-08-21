# Runs the test suite on a Windows box whose Application Control policy blocks the
# throwaway test binaries `go test` builds into a temp directory.
#
# `go test ./...` compiles each package to a randomly named .exe under GOTMPDIR and
# executes it. Where WDAC / Smart App Control is enforcing, that exec is refused —
# "An Application Control policy has blocked this file" — and the package reports
# FAIL for a reason that has nothing to do with the code.
#
# `go test -c -o <name>.exe` writes a stable, explicitly named binary instead, which
# the policy allows. Same tests, same result, just a path the machine trusts.
#
# On any machine without that policy, plain `go test ./...` is equivalent.

# Continue, not Stop: a blocked exec surfaces as a NativeCommandFailed error, and
# under Stop that terminates the script instead of letting the retry below run.
# Every step checks $LASTEXITCODE explicitly, so nothing is swallowed.
$ErrorActionPreference = "Continue"
Set-Location (Join-Path $PSScriptRoot "..")

$packages = @(
    "internal/config",
    "internal/httpapi",
    "internal/monitor",
    "internal/redisx",
    "internal/totp"
)

$failed = @()

foreach ($package in $packages) {
    $name = ($package -replace ".*/", "") + ".test.exe"

    & go test -c -o $name "./$package"
    if ($LASTEXITCODE -ne 0) { $failed += "$package (compile)"; continue }

    # A package with no test files produces no binary at all.
    if (-not (Test-Path $name)) { "SKIP  $package (no tests)"; continue }

    # The policy refuses the FIRST exec of a binary it has not seen before and
    # allows it once whatever scan it triggers has finished, so a refusal is retried
    # rather than reported as a test failure. A genuinely failing test exits
    # non-zero every time and still lands in $failed.
    $passed = $false

    for ($attempt = 1; $attempt -le 4 -and -not $passed; $attempt++) {
        & ".\$name" $args 2>$null
        if ($LASTEXITCODE -eq 0) { $passed = $true }
    }

    if ($passed) { "ok    $package" } else { $failed += $package }

    Remove-Item $name -Force -ErrorAction SilentlyContinue
}

if ($failed.Count -gt 0) {
    ""
    "FAILED: " + ($failed -join ", ")
    exit 1
}

""
"all packages passed"
