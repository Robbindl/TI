#Requires -RunAsAdministrator

param(
    [ValidateSet("install", "pair", "uninstall", "start", "stop", "status")]
    [string]$Action = "install"
)

$ErrorActionPreference = "Stop"
$ServiceName = "WhatsAppTelegramBridge"
$DisplayName = "WhatsApp Telegram Bridge"
$ProjectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinaryPath = Join-Path $ProjectRoot "wa-telegram-bridge.exe"
$DataPath = Join-Path $ProjectRoot "data"
$EnvironmentPath = Join-Path $DataPath "service.env"

function Invoke-Sc {
    param([string[]]$Arguments)
    & sc.exe @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "sc.exe failed with exit code $LASTEXITCODE"
    }
}

function Ensure-EnvironmentFile {
    New-Item -ItemType Directory -Force -Path $DataPath | Out-Null
    if (-not (Test-Path $EnvironmentPath)) {
        $Token = Read-Host "Telegram bot token"
        $OwnerID = Read-Host "Telegram owner ID"
        @(
            "TELEGRAM_BOT_TOKEN=$Token"
            "TELEGRAM_OWNER_ID=$OwnerID"
        ) | Set-Content -Path $EnvironmentPath -Encoding ascii
    }
    icacls $EnvironmentPath /inheritance:r /grant:r "SYSTEM:(R)" "Administrators:(R)" | Out-Null
}

function Build-Bridge {
    Push-Location $ProjectRoot
    try {
        go build -trimpath -o $BinaryPath ./cmd/bridge
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed with exit code $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
    }
}

function Stop-BridgeServiceIfPresent {
    $ExistingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($null -ne $ExistingService -and $ExistingService.Status -ne "Stopped") {
        Stop-Service -Name $ServiceName -Force
        $ExistingService.WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(30))
    }
}

function Import-EnvironmentFile {
    Get-Content $EnvironmentPath | ForEach-Object {
        $line = $_.Trim()
        if ($line -and -not $line.StartsWith("#")) {
            $key, $value = $line.Split("=", 2)
            [Environment]::SetEnvironmentVariable($key.Trim(), $value.Trim().Trim('"'), "Process")
        }
    }
}

switch ($Action) {
    "uninstall" {
        & sc.exe stop $ServiceName 2>$null
        & sc.exe delete $ServiceName
        Write-Host "Service removed. The data folder and credentials were kept."
        break
    }
    "start" {
        Invoke-Sc @("start", $ServiceName)
        break
    }
    "stop" {
        Invoke-Sc @("stop", $ServiceName)
        break
    }
    "status" {
        & sc.exe query $ServiceName
        break
    }
    "pair" {
        Ensure-EnvironmentFile
        Stop-BridgeServiceIfPresent
        Build-Bridge
        Import-EnvironmentFile
        [Environment]::SetEnvironmentVariable("WHATSAPP_FORCE_RELINK", "1", "Process")
        Write-Host "Starting the bridge interactively. Scan the WhatsApp QR code, then press Ctrl+C when pairing is complete."
        & $BinaryPath
        break
    }
    "install" {
        Ensure-EnvironmentFile
        if (-not (Test-Path (Join-Path $DataPath "whatsapp.db"))) {
            throw "WhatsApp is not paired yet. Run .\install-windows-service.ps1 -Action pair first."
        }
        Stop-BridgeServiceIfPresent
        Build-Bridge

        $ServiceCommand = '"' + $BinaryPath + '" -service'
        if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
            Invoke-Sc @("config", $ServiceName, "binPath=", $ServiceCommand, "start=", "auto", "DisplayName=", $DisplayName)
        }
        else {
            Invoke-Sc @("create", $ServiceName, "binPath=", $ServiceCommand, "start=", "auto", "DisplayName=", $DisplayName)
        }
        Invoke-Sc @("description", $ServiceName, "Runs the WhatsApp to Telegram bridge in the background.")
        Invoke-Sc @("failure", $ServiceName, "reset=", "86400", "actions=", "restart/60000/restart/60000/restart/300000")
        Invoke-Sc @("start", $ServiceName)
        Write-Host "Service installed and started: $ServiceName"
        break
    }
}
