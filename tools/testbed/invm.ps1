#Requires -Version 5.1
<#
.SYNOPSIS
Runs a check inside the reference VM through PowerShell Direct.

.DESCRIPTION
The VM, its checkpoint, the administrator credential file and the two Entra users come from
refactor-endpoint/TESTBED.md (node tools/testbed/testbed.mjs). Run it on the owner's PC, elevated
(PowerShell Direct needs Hyper-V administrator rights).

  invm.ps1 -Command '<script>'                    as the VM administrator
  invm.ps1 -AsUser console|second -Command '...'  in that user's own interactive session
  invm.ps1 -Screenshot <out.png>                  the console user's screen, copied to the PC
  invm.ps1 -CopyFrom <vm path> <pc path>
  invm.ps1 -CopyTo <pc path> <vm path>
  invm.ps1 -RestoreCheckpoint                     restores the clean checkpoint, waits for the VM
  invm.ps1 -AgentState                            the agent's health, spool, delivery and policy
#>
[CmdletBinding(DefaultParameterSetName = 'Command')]
param(
    [Parameter(ParameterSetName = 'Command', Mandatory = $true)][string]$Command,
    [Parameter(ParameterSetName = 'Command')][ValidateSet('console', 'second')][string]$AsUser,
    [Parameter(ParameterSetName = 'Screenshot', Mandatory = $true)][string]$Screenshot,
    [Parameter(ParameterSetName = 'CopyFrom', Mandatory = $true)][string]$CopyFrom,
    [Parameter(ParameterSetName = 'CopyTo', Mandatory = $true)][string]$CopyTo,
    [Parameter(ParameterSetName = 'CopyFrom', Mandatory = $true, Position = 0)]
    [Parameter(ParameterSetName = 'CopyTo', Mandatory = $true, Position = 0)][string]$Destination,
    [Parameter(ParameterSetName = 'RestoreCheckpoint', Mandatory = $true)][switch]$RestoreCheckpoint,
    [Parameter(ParameterSetName = 'AgentState', Mandatory = $true)][switch]$AgentState
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$json = & node (Join-Path $PSScriptRoot 'testbed.mjs')
if ($LASTEXITCODE -ne 0) { exit 1 }
$config = ($json -join "`n") | ConvertFrom-Json
$credential = Import-Clixml -Path $config.vmAdminCredential
$UserTimeoutSeconds = 600

# Runs in the VM as its administrator: runs Script as the signed-in Entra user Upn through a one-shot
# scheduled task with an Interactive principal, and returns what it printed.
$runAsUser = {
    param([string]$Upn, [string]$Script, [int]$TimeoutSeconds)
    $ErrorActionPreference = 'Stop'
    $dir = 'C:\ProgramData\SacTestbed'

    # An Entra user's SID, from the identity cache Windows keeps for each account that signed in.
    $sid = $null
    foreach ($key in @(Get-ChildItem -Path 'HKLM:\SOFTWARE\Microsoft\IdentityStore\Cache' -ErrorAction SilentlyContinue)) {
        $cache = Join-Path $key.PSPath ('IdentityCache\' + $key.PSChildName)
        $name = (Get-ItemProperty -Path $cache -ErrorAction SilentlyContinue).UserName
        if ($name -and $name -eq $Upn) { $sid = $key.PSChildName; break }
    }
    if (-not $sid) { throw "$Upn has not signed in on this VM" }
    $owners = @(Get-CimInstance -ClassName Win32_Process -Filter "Name = 'explorer.exe'" |
        ForEach-Object { (Invoke-CimMethod -InputObject $_ -MethodName GetOwnerSid).Sid })
    if ($owners -notcontains $sid) { throw "$Upn has no interactive session on the VM" }

    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $id = [guid]::NewGuid().ToString('N')
    $body = Join-Path $dir "$id.body.ps1"
    $wrapper = Join-Path $dir "$id.ps1"
    $out = Join-Path $dir "$id.out"
    $done = Join-Path $dir "$id.done"
    Set-Content -LiteralPath $body -Value $Script -Encoding UTF8
    Set-Content -LiteralPath $wrapper -Encoding UTF8 -Value @(
        "try { & '$body' *>&1 | Out-File -LiteralPath '$out' -Encoding utf8 -Width 4096 }",
        "finally { Set-Content -LiteralPath '$done' -Value 'done' -Encoding ascii }"
    )
    $task = "SacTestbed-$id"
    $path = '\SacTestbed\'
    $action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$wrapper`""
    $principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Seconds $TimeoutSeconds)
    Register-ScheduledTask -TaskName $task -TaskPath $path -Action $action -Principal $principal -Settings $settings -Force | Out-Null
    try {
        $started = Get-Date
        Start-ScheduledTask -TaskName $task -TaskPath $path
        while (-not (Test-Path -LiteralPath $done)) {
            $elapsed = ((Get-Date) - $started).TotalSeconds
            if ($elapsed -gt 5 -and (Get-ScheduledTask -TaskName $task -TaskPath $path).State -eq 'Ready' -and -not (Test-Path -LiteralPath $done)) {
                $result = (Get-ScheduledTaskInfo -TaskName $task -TaskPath $path).LastTaskResult
                throw ('the task as {0} ended without finishing its script (result 0x{1:X8})' -f $Upn, $result)
            }
            if ($elapsed -gt $TimeoutSeconds) { throw "the task as $Upn did not finish within $TimeoutSeconds seconds" }
            Start-Sleep -Milliseconds 500
        }
        if (Test-Path -LiteralPath $out) { Get-Content -LiteralPath $out -Encoding UTF8 }
    } finally {
        Unregister-ScheduledTask -TaskName $task -TaskPath $path -Confirm:$false -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $body, $wrapper, $out, $done -Force -ErrorAction SilentlyContinue
    }
}

# Runs in the VM: the agent's state from health.json, without key material.
$agentStateBlock = {
    $svc = Get-Service -Name 'ShadowAICapture' -ErrorAction SilentlyContinue
    if ($svc) { 'service         {0}' -f $svc.Status } else { 'service         absent' }
    $file = 'C:\ProgramData\ShadowAICapture\state\health.json'
    if (-not (Test-Path -LiteralPath $file)) { 'health.json     absent'; return }
    $h = Get-Content -LiteralPath $file -Raw | ConvertFrom-Json
    'snapshot        {0}' -f $h.generated_at
    'agent           {0}  enrolled {1}  device {2}  managed_state {3}  mode {4}' -f $h.agent_version, $h.enrolled, $h.device_id, $h.managed_state, $h.collection_mode
    'policy bundle   {0}  outcome {1} {2}' -f $h.policy_version, $h.policy_outcome, $h.policy_cause
    if ($h.policy_fetch) { 'policy fetch    last {0}  answer {1}  {2}' -f $h.policy_fetch.last_fetch_at, $h.policy_fetch.last_answer, $h.policy_fetch.last_error }
    'health ack      last {0}  reports {1}  {2}' -f $h.last_heartbeat_at, $h.heartbeats_published, $h.last_heartbeat_error
    'delivery        {0} {1}  last success {2}' -f $h.drain.state, $h.drain.detail, $h.drain.last_success_at
    'spool           depth {0}  dropped_total {1}  rejected_total {2}  delivered_total {3}  oldest {4}' -f $h.spool.spool_depth, $h.spool.spool_dropped_total, $h.spool.spool_rejected_total, $h.spool.spool_delivered_total, $h.spool.oldest_spooled_at
    'health rows'
    $rows = @($h.classifier) + @($h.reports) + @($h.extension) | Where-Object { $_ }
    foreach ($r in $rows) {
        '  {0,-26} {1,-9} {2,-26} last success {3}' -f $r.collector, $r.state, $r.detail, $r.last_success_at
        if ($r.counters) {
            '      ' + ((@($r.counters.PSObject.Properties) | Sort-Object Name | ForEach-Object { '{0}={1}' -f $_.Name, $_.Value }) -join ' ')
        }
    }
}

function Invoke-AsUser([string]$Which, [string]$Script) {
    $upn = $config.consoleUser
    if ($Which -eq 'second') { $upn = $config.secondUser }
    Invoke-Command -VMName $config.vmName -Credential $credential -ScriptBlock $runAsUser -ArgumentList $upn, $Script, $UserTimeoutSeconds
}

function Use-Session([scriptblock]$Action) {
    $session = New-PSSession -VMName $config.vmName -Credential $credential
    try { & $Action $session } finally { Remove-PSSession -Session $session }
}

function Resolve-PCPath([string]$Path) {
    $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
}

switch ($PSCmdlet.ParameterSetName) {
    'Command' {
        if ($AsUser) {
            Invoke-AsUser $AsUser $Command
        } else {
            Invoke-Command -VMName $config.vmName -Credential $credential -ScriptBlock ([scriptblock]::Create($Command)) -ErrorAction Continue -ErrorVariable remoteErrors
            if ($remoteErrors.Count) { exit 1 }
        }
    }
    'Screenshot' {
        $target = Resolve-PCPath $Screenshot
        $vmFile = 'C:\ProgramData\SacTestbed\screen-' + [guid]::NewGuid().ToString('N') + '.png'
        $capture = @'
Add-Type -AssemblyName System.Windows.Forms, System.Drawing
Add-Type -Namespace SacTestbed -Name Display -MemberDefinition '[DllImport("user32.dll")] public static extern bool SetProcessDPIAware();'
[SacTestbed.Display]::SetProcessDPIAware() | Out-Null
$bounds = [System.Windows.Forms.SystemInformation]::VirtualScreen
$bitmap = New-Object System.Drawing.Bitmap $bounds.Width, $bounds.Height
$graphics = [System.Drawing.Graphics]::FromImage($bitmap)
$graphics.CopyFromScreen($bounds.Left, $bounds.Top, 0, 0, $bitmap.Size)
$bitmap.Save('__FILE__', [System.Drawing.Imaging.ImageFormat]::Png)
$graphics.Dispose()
$bitmap.Dispose()
'@
        Invoke-AsUser 'console' $capture.Replace('__FILE__', $vmFile)
        Use-Session {
            param($session)
            Copy-Item -FromSession $session -Path $vmFile -Destination $target -Force
            Invoke-Command -Session $session -ScriptBlock { param($p) Remove-Item -LiteralPath $p -Force } -ArgumentList $vmFile
        }
        "screenshot: $target"
    }
    'CopyFrom' {
        $target = Resolve-PCPath $Destination
        Use-Session { param($session) Copy-Item -FromSession $session -Path $CopyFrom -Destination $target -Recurse -Force }
    }
    'CopyTo' {
        $source = Resolve-PCPath $CopyTo
        Use-Session { param($session) Copy-Item -ToSession $session -Path $source -Destination $Destination -Recurse -Force }
    }
    'RestoreCheckpoint' {
        Restore-VMSnapshot -VMName $config.vmName -Name $config.cleanCheckpoint -Confirm:$false
        if ((Get-VM -Name $config.vmName).State -ne 'Running') { Start-VM -Name $config.vmName }
        $deadline = (Get-Date).AddMinutes(10)
        while ($true) {
            try {
                Invoke-Command -VMName $config.vmName -Credential $credential -ScriptBlock { $env:COMPUTERNAME } -ErrorAction Stop | Out-Null
                break
            } catch {
                if ((Get-Date) -gt $deadline) { throw "PowerShell Direct did not answer within 10 minutes of restoring '$($config.cleanCheckpoint)'" }
                Start-Sleep -Seconds 5
            }
        }
        "restored '$($config.cleanCheckpoint)'; PowerShell Direct answers"
    }
    'AgentState' {
        Invoke-Command -VMName $config.vmName -Credential $credential -ScriptBlock $agentStateBlock
    }
}
