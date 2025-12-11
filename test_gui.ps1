# Windows Test Script: Compile and Test GUI Installer

Write-Host "=== Compiling GUI Installer ==="
Write-Host "Current directory: $(Get-Location)"

# 1. Compile stub (with GUI, no console window)
Write-Host "Compiling stub.exe..."
go build -ldflags="-H=windowsgui" -o installer/stub/stub.exe ./installer/stub
if ($LASTEXITCODE -ne 0) {
    Write-Host "stub compilation failed!" -ForegroundColor Red
    exit 1
}

# 2. Compile main packager
Write-Host "Compiling main.exe..."
go build -o main.exe
if ($LASTEXITCODE -ne 0) {
    Write-Host "main compilation failed!" -ForegroundColor Red
    exit 1
}

# 3. Test packaging (download mode)
Write-Host "Creating installer..."
./main.exe
if ($LASTEXITCODE -ne 0) {
    Write-Host "Installer creation failed!" -ForegroundColor Red
    exit 1
}

Write-Host "Installer created successfully: lol_yuumi_setup_v091.exe" -ForegroundColor Green
Write-Host "You can run it to test the GUI interface."
Write-Host "Note: The installer will download yuumi.exe from the configured URL during installation." -ForegroundColor Cyan

Write-Host "=== Test Completed ===" -ForegroundColor Green