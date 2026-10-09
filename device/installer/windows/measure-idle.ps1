<#
.SYNOPSIS
  Measure capture-core's idle CPU and memory against the agent's budget.

.DESCRIPTION
  Samples capture-core and every process it started (classifier-host, the user-session helpers)
  for 10 minutes with Get-Counter, then prints the average CPU of the whole tree as a share of
  one core and the peak private working set of the whole tree. It exits 1 when either is at or
  above the budget: under 1 % of one core averaged over the 10 minutes, and a private working set
  under 150 MB.

  The tree is the ShadowAICapture service's process, or the capture-core process given by
  -ProcessId, and its descendants, looked up again at every sample so that a restarted
  classifier-host or a helper started for a new session is counted.

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File device/installer/windows/measure-idle.ps1

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File device/installer/windows/measure-idle.ps1 -ProcessId 4242
#>
[CmdletBinding()]
param(
  # A capture-core started by hand instead of the service.
  [int]$ProcessId = 0
)

$ErrorActionPreference = 'Stop'

$cpuLimitPercent = 1
$memLimitBytes = 150MB
$interval = 2
$samples = 300 # 10 minutes at 2 s

if ($ProcessId -eq 0) {
  $svc = Get-CimInstance Win32_Service -Filter "Name = 'ShadowAICapture'"
  if ($null -eq $svc -or $svc.ProcessId -eq 0) { throw 'The ShadowAICapture service is not running; start it or pass -ProcessId.' }
  $ProcessId = [int]$svc.ProcessId
}
$root = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
if ($null -eq $root) { throw "No process has id $ProcessId." }
if ($root.ProcessName -ne 'capture-core') { throw "Process $ProcessId is $($root.ProcessName), not capture-core." }

# The root and its descendants. A process whose parent id was reused by a later process is not a
# descendant: a child is never older than its parent.
function Get-Tree {
  $all = @(Get-CimInstance Win32_Process -Property ProcessId, ParentProcessId, CreationDate)
  $byId = @{}
  foreach ($p in $all) { $byId[[int]$p.ProcessId] = $p }
  if (-not $byId.ContainsKey($ProcessId)) { throw "capture-core (process $ProcessId) exited during the measurement." }
  $tree = @{ $ProcessId = $true }
  $grew = $true
  while ($grew) {
    $grew = $false
    foreach ($p in $all) {
      $id = [int]$p.ProcessId
      $parent = [int]$p.ParentProcessId
      if (-not $tree.ContainsKey($id) -and $tree.ContainsKey($parent) -and $p.CreationDate -ge $byId[$parent].CreationDate) {
        $tree[$id] = $true
        $grew = $true
      }
    }
  }
  return $tree
}

$counters = '\Process(*)\ID Process', '\Process(*)\% Processor Time', '\Process(*)\Working Set - Private'
$cpuSum = 0.0
$memPeak = 0.0
$taken = 0
$names = @{}

Write-Host "Sampling capture-core (process $ProcessId) and its children every $interval s for $($samples * $interval / 60) minutes..."
# A process that exits between listing the instances and reading them makes that one value
# invalid; it is skipped, not fatal.
Get-Counter -Counter $counters -SampleInterval $interval -MaxSamples $samples -ErrorAction SilentlyContinue | ForEach-Object {
  $tree = Get-Tree
  $pidOf = @{}
  $cpu = @{}
  $mem = @{}
  foreach ($s in $_.CounterSamples) {
    if ($s.Status -ne 0) { continue }
    switch (($s.Path -split '\\')[-1]) {
      'id process' { $pidOf[$s.InstanceName] = [int]$s.CookedValue }
      '% processor time' { $cpu[$s.InstanceName] = $s.CookedValue }
      'working set - private' { $mem[$s.InstanceName] = $s.CookedValue }
    }
  }
  $sampleCpu = 0.0
  $sampleMem = 0.0
  foreach ($instance in $pidOf.Keys) {
    if (-not $tree.ContainsKey($pidOf[$instance])) { continue }
    $names[($instance -replace '#\d+$', '')] = $true
    if ($cpu.ContainsKey($instance)) { $sampleCpu += $cpu[$instance] }
    if ($mem.ContainsKey($instance)) { $sampleMem += $mem[$instance] }
  }
  # % Processor Time is a share of one core, so the tree's sum is too.
  $cpuSum += $sampleCpu
  $memPeak = [Math]::Max($memPeak, $sampleMem)
  $taken++
}

if ($taken -eq 0) { throw 'Get-Counter returned no samples.' }
$cpuAvg = $cpuSum / $taken
Write-Host ("Processes: {0}; {1} samples" -f (($names.Keys | Sort-Object) -join ', '), $taken)
Write-Host ("Average CPU: {0:N3} % of one core (budget under {1} %)" -f $cpuAvg, $cpuLimitPercent)
Write-Host ("Peak private working set: {0:N1} MB (budget under {1} MB)" -f ($memPeak / 1MB), ($memLimitBytes / 1MB))

if ($cpuAvg -ge $cpuLimitPercent -or $memPeak -ge $memLimitBytes) {
  Write-Host 'Over budget.'
  exit 1
}
Write-Host 'Within budget.'
exit 0
