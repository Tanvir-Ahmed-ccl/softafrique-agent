[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string[]]$Files,
    [string]$TimestampUrl = 'http://timestamp.digicert.com'
)

$ErrorActionPreference = 'Stop'

$pfxBase64 = $env:SOFTAFRIQUE_SIGNING_PFX_BASE64
$pfxPassword = $env:SOFTAFRIQUE_SIGNING_PFX_PASSWORD

if ([string]::IsNullOrWhiteSpace($pfxBase64)) {
    Write-Host 'signing skipped: SOFTAFRIQUE_SIGNING_PFX_BASE64 is not set'
    exit 0
}
if ([string]::IsNullOrWhiteSpace($pfxPassword)) {
    throw 'SOFTAFRIQUE_SIGNING_PFX_BASE64 is set but SOFTAFRIQUE_SIGNING_PFX_PASSWORD is not'
}

$signtool = Get-Command signtool.exe -ErrorAction SilentlyContinue |
    Select-Object -First 1 -ExpandProperty Source
if (-not $signtool) {
    $kits = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\bin'
    if (Test-Path $kits) {
        $signtool = Get-ChildItem -Path $kits -Filter signtool.exe -Recurse -File |
            Where-Object { $_.DirectoryName -match '\\x64$' } |
            Sort-Object -Property FullName -Descending |
            Select-Object -First 1 -ExpandProperty FullName
    }
}
if (-not $signtool) {
    throw 'signtool.exe was not found; install the Windows SDK'
}

$pfxBase64 = $pfxBase64 -replace '\s', ''
$tempDir = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }
$pfxPath = Join-Path $tempDir ("softafrique-signing-{0}.pfx" -f [guid]::NewGuid().ToString('N'))
$thumbprint = $null

try {
    [IO.File]::WriteAllBytes($pfxPath, [Convert]::FromBase64String($pfxBase64))
    $securePassword = ConvertTo-SecureString -String $pfxPassword -AsPlainText -Force
    $imported = @(Import-PfxCertificate -FilePath $pfxPath -CertStoreLocation Cert:\CurrentUser\My -Password $securePassword) |
        Where-Object { $_.HasPrivateKey } |
        Select-Object -First 1
    if (-not $imported) {
        throw 'the imported signing certificate has no private key'
    }
    $thumbprint = $imported.Thumbprint

    foreach ($file in $Files) {
        $path = (Resolve-Path -LiteralPath $file).Path
        & $signtool sign /fd SHA256 /tr $TimestampUrl /td SHA256 /sha1 $thumbprint $path
        if ($LASTEXITCODE -ne 0) {
            throw "signtool sign failed for $path with exit code $LASTEXITCODE"
        }
        & $signtool verify /pa /v $path
        if ($LASTEXITCODE -ne 0) {
            throw "signtool verify failed for $path with exit code $LASTEXITCODE"
        }
        Write-Host "signed and verified $path"
    }
}
finally {
    if ($thumbprint) {
        Remove-Item -LiteralPath "Cert:\CurrentUser\My\$thumbprint" -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -LiteralPath $pfxPath -Force -ErrorAction SilentlyContinue
}
