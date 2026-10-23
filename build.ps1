# Builds release binaries into .\dist (run from PowerShell on Windows).
#   .\build.ps1            -> linux/amd64, linux/arm64, windows/amd64 (CLI)
#   .\build.ps1 -Version 0.2.0
param([string]$Version = "0.2.0")

$ErrorActionPreference = "Stop"
$env:GOTOOLCHAIN = "local"
$env:CGO_ENABLED = "0"
$ldflags = "-s -w -X github.com/thisisnabi/Nube/internal/version.Version=$Version"
New-Item -ItemType Directory -Force dist | Out-Null

$targets = @(
    @{ os = "linux";   arch = "amd64"; out = "dist/nube-linux-amd64" },
    @{ os = "linux";   arch = "arm64"; out = "dist/nube-linux-arm64" },
    @{ os = "windows"; arch = "amd64"; out = "dist/nube.exe" }
)
foreach ($t in $targets) {
    $env:GOOS = $t.os; $env:GOARCH = $t.arch
    go build -trimpath -ldflags $ldflags -o $t.out ./cmd/nube
    if ($LASTEXITCODE -ne 0) { throw "build failed for $($t.os)/$($t.arch)" }
    Write-Host "built $($t.out)"
}
Remove-Item Env:GOOS, Env:GOARCH

# A ready-to-copy bundle for the Ubuntu machine.
$bundle = "dist/nube-$Version-linux"
New-Item -ItemType Directory -Force $bundle | Out-Null
Copy-Item dist/nube-linux-amd64, dist/nube-linux-arm64 $bundle -Force
Copy-Item deploy/install.sh $bundle -Force
Copy-Item -Recurse -Force deploy/systemd $bundle
Copy-Item -Recurse -Force examples $bundle
tar -czf "dist/nube-$Version-linux.tar.gz" -C dist "nube-$Version-linux"
Write-Host "bundle: dist/nube-$Version-linux.tar.gz"
