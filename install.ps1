# Install the latest Grimoire release on Windows.
#
#   irm https://raw.githubusercontent.com/JeremiahM37/grimoire/main/install.ps1 | iex
#
# Downloads the zip for this CPU from the latest GitHub release, verifies it
# against checksums.txt, unpacks it whole (the console and plugins are files
# beside the binary) under $env:LOCALAPPDATA\Programs\grimoire and adds that
# directory to the user PATH.
$ErrorActionPreference = "Stop"
$repo = "JeremiahM37/grimoire"
$version = if ($env:GRIMOIRE_VERSION) { $env:GRIMOIRE_VERSION } else { "latest" }
$arch = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq "Arm64") { "arm64" } else { "amd64" }
$base = if ($version -eq "latest") { "https://github.com/$repo/releases/latest/download" } else { "https://github.com/$repo/releases/download/$version" }
$archive = "grimoire_windows_$arch.zip"
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("grimoire-" + [System.Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "Downloading $archive ($version)…"
  Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
  Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp "checksums.txt")
  $want = (Get-Content (Join-Path $tmp "checksums.txt") | Where-Object { $_ -match " $archive$" }) -split " " | Select-Object -First 1
  $got = (Get-FileHash (Join-Path $tmp $archive) -Algorithm SHA256).Hash.ToLower()
  if (-not $want -or $want -ne $got) { throw "checksum mismatch for $archive" }
  $dir = if ($env:GRIMOIRE_INSTALL_DIR) { $env:GRIMOIRE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\grimoire" }
  $new = "$dir.new"
  if (Test-Path $new) { Remove-Item -Recurse -Force $new }
  Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $new -Force
  if (Test-Path $dir) { Remove-Item -Recurse -Force $dir }
  Move-Item $new $dir
  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  if (($userPath -split ";") -notcontains $dir) {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$dir", "User")
    $env:Path = "$env:Path;$dir"
    Write-Host "Added $dir to your user PATH (open a new terminal to pick it up)."
  }
  Write-Host ("Installed " + (& (Join-Path $dir "grimoire.exe") version) + " to $dir")
  Write-Host "Next: set GRIMOIRE_VAULT to your notes folder and run grimoire; the console is at http://localhost:9111"
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
