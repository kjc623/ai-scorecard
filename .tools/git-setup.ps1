<#
  Point this repository at its Desktop backup, idempotently.

  Creates a bare repository, registers it as the `backup` remote, pushes
  everything, and installs the post-commit hook that keeps it current. Safe to
  re-run: existing pieces are detected and left alone.

      powershell -ExecutionPolicy Bypass -File .tools\git-setup.ps1
      powershell -ExecutionPolicy Bypass -File .tools\git-setup.ps1 -Destination "D:\elsewhere"

  Why a bare repository rather than a working copy: a clone would need its own
  checkout, and a second working tree invites edits in the wrong place. A bare
  repo is a complete copy of the history and every branch, and `git log`,
  `git show` and `git restore --source` all work against it directly.
#>

[CmdletBinding()]
param(
  # Where the backup lives. Defaults to a folder on the Desktop.
  [string] $Destination = (Join-Path ([Environment]::GetFolderPath('Desktop')) 'git_backup'),
  # Name of the remote in this repository.
  [string] $Remote = 'backup'
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$bare = Join-Path $Destination 'architecture.git'

function Say($message) { Write-Host "  $message" }

Write-Host "`nProject Cockpit backup setup`n"

Push-Location $repo
try {
  if (-not (Test-Path (Join-Path $repo '.git'))) {
    throw "$repo is not a git repository. Run 'git init' first."
  }

  # --- identity -------------------------------------------------------------
  # A commit fails outright without one, which is the first thing that bites
  # after a fresh `git init`.
  if (-not (git config user.name))  { git config user.name  'lead' }
  if (-not (git config user.email)) { git config user.email 'lead@local' }
  Say ("identity: {0} <{1}>" -f (git config user.name), (git config user.email))

  # --- the bare copy --------------------------------------------------------
  if (-not (Test-Path $bare)) {
    New-Item -ItemType Directory -Force -Path $Destination | Out-Null
    git init --bare --initial-branch=master $bare | Out-Null
    Say "created $bare"
  } else {
    Say "already present: $bare"
  }
  git -C $bare config core.logAllRefUpdates true | Out-Null

  # --- the remote -----------------------------------------------------------
  $existing = git remote get-url $Remote 2>$null
  if ($LASTEXITCODE -ne 0 -or -not $existing) {
    git remote add $Remote $bare
    Say "remote '$Remote' -> $bare"
  } elseif ($existing -ne $bare) {
    git remote set-url $Remote $bare
    Say "remote '$Remote' re-pointed to $bare"
  } else {
    Say "remote '$Remote' already correct"
  }

  # --- the hook -------------------------------------------------------------
  # Two hooks, because Git for Windows runs hooks through MSYS sh.exe: the .cmd
  # twin covers environments where sh.exe cannot start (notably inside a DSH
  # session, whose sandbox denies it the kernel objects it needs).
  $hooksDir = Join-Path $repo '.git\hooks'
  New-Item -ItemType Directory -Force -Path $hooksDir | Out-Null
  foreach ($hook in @('post-commit', 'post-commit.cmd')) {
    $from = Join-Path $PSScriptRoot "git-hooks\$hook"
    if (-not (Test-Path $from)) { continue }
    $text = [System.IO.File]::ReadAllText($from).Replace("`r`n", "`n")
    # No BOM: a shebang must be the first byte or sh refuses the file.
    [System.IO.File]::WriteAllText((Join-Path $hooksDir $hook), $text, (New-Object System.Text.UTF8Encoding($false)))
    Say "installed hook: $hook"
  }

  # --- first sync -----------------------------------------------------------
  $branch = git symbolic-ref --short -q HEAD
  if (-not $branch) { throw 'Detached HEAD: check out a branch before syncing.' }
  git push --quiet -u $Remote $branch
  if ($LASTEXITCODE -ne 0) { throw "Initial push to '$Remote' failed." }
  git push --quiet $Remote --tags 2>$null

  $local = (git rev-parse HEAD).Trim()
  $there = (git --git-dir $bare rev-parse $branch).Trim()
  Say ("{0}: {1} -> {2}" -f $branch, $local.Substring(0, 7), $there.Substring(0, 7))

  if ($local -ne $there) { throw 'The backup does not match the local branch after pushing.' }

  Write-Host "`nBackup is current. Every commit from a normal terminal pushes to it automatically."
  Write-Host "To sync on demand:  powershell -ExecutionPolicy Bypass -File .tools\git-sync.ps1`n"
} finally {
  Pop-Location
}
