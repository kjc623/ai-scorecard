<#
  Keep the Desktop backup current.

  The post-commit hook does this automatically when you commit from a normal
  terminal. This script is the belt-and-braces: run it whenever, or let the
  scheduled task (installed by .tools/git-setup.ps1) run it for you.

      pwsh -File .tools\git-sync.ps1

  It commits nothing and changes no history - it pushes what you already have.
#>

$ErrorActionPreference = 'Stop'

$repo = Split-Path -Parent $PSScriptRoot
$remote = 'backup'

Push-Location $repo
try {
  $url = git remote get-url $remote 2>$null
  if ($LASTEXITCODE -ne 0) {
    Write-Host "No '$remote' remote configured in $repo - nothing to sync."
    exit 0
  }

  $branch = (git symbolic-ref --short -q HEAD 2>$null)
  if (-not $branch) {
    Write-Host 'Detached HEAD: pushing every branch instead.'
    git push --quiet $remote --all
  } else {
    git push --quiet $remote $branch
    if ($LASTEXITCODE -ne 0) {
      Write-Host "Push of '$branch' failed. The commits are safe locally; retry when the backup is reachable."
      exit 1
    }
  }
  git push --quiet $remote --tags 2>$null

  $local = (git rev-parse HEAD).Trim()
  $there = (git --git-dir $url rev-parse $branch 2>$null).Trim()
  $state = if ($local -eq $there) { 'up to date' } else { 'BEHIND - rerun' }
  Write-Host ("{0}: {1} -> {2} ({3})" -f $branch, $local.Substring(0, 7), $there.Substring(0, 7), $state)
  if ($local -ne $there) { exit 1 }
} finally {
  Pop-Location
}
