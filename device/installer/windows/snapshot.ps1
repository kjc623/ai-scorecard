<#
.SYNOPSIS
  Snapshot every location Shadow AI Capture changes outside its own folders, or compare two
  snapshots.

.DESCRIPTION
  An uninstall must leave the machine as the agent found it. Taken before the install and again after
  the uninstall, two snapshots show whether it did. A snapshot records, as one flat map of location to
  value:

    file:<path>          the SHA-256 of each tool's managed file the agent writes and of the
                         Start-menu shortcut, or "absent"
    hku:<sid>\<name>     each value of Internet Settings in every loaded user hive
    env:<name>           each value of the machine environment
    policy:<name>        each value of VS Code's machine policies (Copilot's managed settings)
    root:<thumbprint>    each certificate in the machine Root store, with its subject
    cngkey:<name>        each machine key of the Microsoft Software Key Storage Provider
    firewall:<name>      each firewall rule name, with how many rules carry it

  It reads every loaded user hive, so run it as an administrator while the users whose settings
  matter are signed in. It changes nothing.

  -Compare prints what differs between two snapshots, "+" for a location only the second has, "-"
  for one only the first has and "~" for a changed value, and exits 1 when anything differs.

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File snapshot.ps1 -Out before.json

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File snapshot.ps1 -Compare before.json after.json
#>
[CmdletBinding(DefaultParameterSetName = 'Snapshot')]
param(
  [Parameter(ParameterSetName = 'Snapshot')][string]$Out = '',
  [Parameter(ParameterSetName = 'Compare', Mandatory = $true)][switch]$Compare,
  [Parameter(ParameterSetName = 'Compare', Mandatory = $true, Position = 0)][string]$Before,
  [Parameter(ParameterSetName = 'Compare', Mandatory = $true, Position = 1)][string]$After
)

$ErrorActionPreference = 'Stop'

# The files the agent writes: the tools' managed configuration (capture-core/toolconfig) and the
# Start-menu shortcut the MSI installs.
$files = @(
  (Join-Path $env:ProgramFiles 'ClaudeCode\managed-settings.json'),
  (Join-Path $env:ProgramData 'Cursor\hooks.json'),
  (Join-Path $env:ProgramData 'OpenAI\Codex\requirements.toml'),
  (Join-Path $env:ProgramData 'OpenAI\Codex\config.toml'),
  (Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\Shadow AI Capture.lnk')
)
$internetSettings = 'Software\Microsoft\Windows\CurrentVersion\Internet Settings'
$machineEnvironment = 'HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\Session Manager\Environment'
$vsCodePolicies = 'HKEY_LOCAL_MACHINE\SOFTWARE\Policies\Microsoft\VSCode'

function Read-Snapshot([string]$path) {
  $snap = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
  $map = @{}
  foreach ($p in $snap.entries.PSObject.Properties) { $map[$p.Name] = [string]$p.Value }
  return $map
}

if ($Compare) {
  $a = Read-Snapshot $Before
  $b = Read-Snapshot $After
  $keys = @($a.Keys) + @($b.Keys) | Sort-Object -Unique
  $differences = 0
  foreach ($k in $keys) {
    if (-not $b.ContainsKey($k)) { Write-Output "- $k = $($a[$k])"; $differences++ }
    elseif (-not $a.ContainsKey($k)) { Write-Output "+ $k = $($b[$k])"; $differences++ }
    elseif ($a[$k] -cne $b[$k]) { Write-Output "~ $k`: $($a[$k]) -> $($b[$k])"; $differences++ }
  }
  if ($differences -gt 0) {
    Write-Output "$differences difference(s) between $Before and $After"
    exit 1
  }
  Write-Output "no differences between $Before and $After ($($a.Count) locations)"
  exit 0
}

$entries = @{}

# A registry value as kind and data, so a change of either shows.
function Format-RegistryValue($key, [string]$name) {
  $kind = $key.GetValueKind($name)
  $data = $key.GetValue($name, $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
  switch ($kind) {
    'Binary' { $text = [BitConverter]::ToString([byte[]]$data) }
    'MultiString' { $text = ([string[]]$data) -join '|' }
    default { $text = [string]$data }
  }
  return "${kind}:$text"
}

function Add-RegistryValues([string]$prefix, [string]$path) {
  $key = Get-Item -LiteralPath "Registry::$path" -ErrorAction SilentlyContinue
  if ($null -eq $key) { return }
  foreach ($name in $key.GetValueNames()) {
    $entries["$prefix$name"] = Format-RegistryValue $key $name
  }
}

foreach ($f in $files) {
  if (Test-Path -LiteralPath $f -PathType Leaf) {
    $entries["file:$f"] = (Get-FileHash -LiteralPath $f -Algorithm SHA256).Hash
  } else {
    $entries["file:$f"] = 'absent'
  }
}

# The loaded hives of people (local, Entra and Microsoft accounts), as the desktop-app PAC picks them.
foreach ($hive in Get-ChildItem -LiteralPath 'Registry::HKEY_USERS') {
  $sid = $hive.PSChildName
  if ($sid -like '*_Classes') { continue }
  if ($sid -notmatch '^S-1-(5-21|12-1|11)-') { continue }
  Add-RegistryValues "hku:$sid\" "HKEY_USERS\$sid\$internetSettings"
}

Add-RegistryValues 'env:' $machineEnvironment
Add-RegistryValues 'policy:' $vsCodePolicies

foreach ($cert in Get-ChildItem -LiteralPath 'Cert:\LocalMachine\Root') {
  $entries["root:$($cert.Thumbprint)"] = $cert.Subject
}

Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;

public static class SacCngKeys {
  [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
  struct NCryptKeyName {
    public IntPtr pszName;
    public IntPtr pszAlgid;
    public int dwLegacyKeySpec;
    public int dwFlags;
  }

  [DllImport("ncrypt.dll", CharSet = CharSet.Unicode)]
  static extern int NCryptOpenStorageProvider(out IntPtr phProvider, string pszProviderName, int dwFlags);
  [DllImport("ncrypt.dll")]
  static extern int NCryptEnumKeys(IntPtr hProvider, IntPtr pszScope, out IntPtr ppKeyName, ref IntPtr ppEnumState, int dwFlags);
  [DllImport("ncrypt.dll")]
  static extern int NCryptFreeBuffer(IntPtr pvInput);
  [DllImport("ncrypt.dll")]
  static extern int NCryptFreeObject(IntPtr hObject);

  const int MachineKey = 0x20;           // NCRYPT_MACHINE_KEY_FLAG
  const int Silent = 0x40;               // NCRYPT_SILENT_FLAG
  const int NoMoreItems = unchecked((int)0x8009002A); // NTE_NO_MORE_ITEMS

  // The names of the machine keys the Microsoft Software Key Storage Provider holds.
  public static string[] MachineKeyNames() {
    IntPtr provider;
    int status = NCryptOpenStorageProvider(out provider, "Microsoft Software Key Storage Provider", 0);
    if (status != 0) throw new Exception(String.Format("NCryptOpenStorageProvider: 0x{0:X8}", status));
    List<string> names = new List<string>();
    IntPtr state = IntPtr.Zero;
    try {
      while (true) {
        IntPtr item;
        status = NCryptEnumKeys(provider, IntPtr.Zero, out item, ref state, MachineKey | Silent);
        if (status == NoMoreItems) break;
        if (status != 0) throw new Exception(String.Format("NCryptEnumKeys: 0x{0:X8}", status));
        NCryptKeyName key = (NCryptKeyName)Marshal.PtrToStructure(item, typeof(NCryptKeyName));
        names.Add(Marshal.PtrToStringUni(key.pszName));
        NCryptFreeBuffer(item);
      }
    } finally {
      if (state != IntPtr.Zero) NCryptFreeBuffer(state);
      NCryptFreeObject(provider);
    }
    return names.ToArray();
  }
}
'@
foreach ($name in [SacCngKeys]::MachineKeyNames()) {
  $entries["cngkey:$name"] = 'present'
}

$firewall = New-Object -ComObject HNetCfg.FwPolicy2
foreach ($group in ($firewall.Rules | ForEach-Object { $_.Name } | Group-Object)) {
  $entries["firewall:$($group.Name)"] = [string]$group.Count
}
[System.Runtime.InteropServices.Marshal]::ReleaseComObject($firewall) | Out-Null

$sorted = [ordered]@{}
foreach ($k in ($entries.Keys | Sort-Object)) { $sorted[$k] = $entries[$k] }
$json = [ordered]@{
  taken_at = (Get-Date).ToUniversalTime().ToString('o')
  computer = $env:COMPUTERNAME
  entries  = $sorted
} | ConvertTo-Json -Depth 4

if ($Out) {
  [System.IO.File]::WriteAllText($Out, $json, (New-Object System.Text.UTF8Encoding $false))
  Write-Output "$($sorted.Count) locations written to $Out"
} else {
  Write-Output $json
}
