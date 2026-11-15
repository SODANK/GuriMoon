# rebuild.ps1 - build the GuriMoon server with every module in modules\.
#
#   rebuild.ps1           build WITH modules, replace gurimoon.exe
#   rebuild.ps1 check     compile check WITH modules -> build\gurimoon-check.exe
#                         (the live gurimoon.exe is not touched)
#   rebuild.ps1 core      plain module-less build, replaces gurimoon.exe
#
# Module discovery (modules\<name>\):
#   - has mod-rebuild.ps1    -> OVERLAY module. Called with -OverlayOnly
#                               -ServerRepo <here>; it writes its own
#                               build\overlay.json (added files, its virtual
#                               source tree) and this script merges every
#                               module's overlay into one.
#   - has .go files only     -> IN-TREE module, blank-imported as
#                               gurimoon/modules/<name>.
#   - skeleton\, or any folder holding a file named .disabled -> skipped.
#   - may ship build-tags.txt (one Go build tag per line); every enabled
#     module's tags are passed to go build -tags.
#
# modules\modules.go is owned here: each overlay module maps it to a file of
# blank imports, and those imports are collected into one generated file.
# Any OTHER path claimed by two modules stops the build, because an overlay
# maps a file exactly once.
#
# ASCII only in this file: it has no BOM and PowerShell 5.1 reads it as ANSI.

param([string]$Mode = "")

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$BuildDir = Join-Path $Root "build"
$Mode = $Mode.ToLowerInvariant()

if ($Mode -notin @("", "check", "core")) {
    Write-Host "Unknown mode '$Mode'. Use: (none) | check | core"
    exit 1
}

function Fail([string]$msg) {
    Write-Host "=== BUILD FAILED: $msg ==="
    exit 1
}

# -- Module discovery ----------------------------------------------------------
$imports = New-Object System.Collections.Generic.List[string]
$included = New-Object System.Collections.Generic.List[string]
$tags = New-Object System.Collections.Generic.List[string]
$replace = [ordered]@{}   # overlay target (as given) -> replacement path
$owner = @{}              # lowercased target -> module name, for conflicts
$modulesGo = Join-Path $Root "modules\modules.go"

if ($Mode -ne "core") {
    foreach ($dir in Get-ChildItem -Path (Join-Path $Root "modules") -Directory) {
        $name = $dir.Name
        if ($name -eq "skeleton") { continue }
        if (Test-Path (Join-Path $dir.FullName ".disabled")) {
            Write-Host "module $name : disabled (.disabled present), skipped"
            continue
        }

        # Build tags the module asks for (one per line, # comments). Lets one
        # module compile code that exists only when another is present.
        $tagFile = Join-Path $dir.FullName "build-tags.txt"
        if (Test-Path $tagFile) {
            foreach ($line in [System.IO.File]::ReadAllLines($tagFile)) {
                $t = $line.Trim()
                if ($t -eq "" -or $t.StartsWith("#")) { continue }
                if ($t -notmatch '^[A-Za-z0-9_.]+$') { Fail "module $name : bad build tag '$t' in build-tags.txt" }
                if (-not $tags.Contains($t)) { $tags.Add($t) }
            }
        }

        $script = Join-Path $dir.FullName "mod-rebuild.ps1"
        if (Test-Path $script) {
            Write-Host "=== module $name : collecting overlay ==="
            try {
                & $script -OverlayOnly -ServerRepo $Root
            } catch {
                Fail "module $name overlay step: $($_.Exception.Message)"
            }
            $ovPath = Join-Path $dir.FullName "build\overlay.json"
            if (-not (Test-Path $ovPath)) { Fail "module $name wrote no build\overlay.json" }
            $ov = [System.IO.File]::ReadAllText($ovPath) | ConvertFrom-Json

            $sawModulesGo = $false
            foreach ($p in $ov.Replace.PSObject.Properties) {
                $target = $p.Name
                $source = $p.Value
                if ($target -ieq $modulesGo) {
                    # The module's enable file: harvest its blank imports.
                    $sawModulesGo = $true
                    $txt = [System.IO.File]::ReadAllText($source)
                    foreach ($m in [regex]::Matches($txt, '_\s+"([^"]+)"')) {
                        $imp = $m.Groups[1].Value
                        if (-not $imports.Contains($imp)) { $imports.Add($imp) }
                    }
                    continue
                }
                $key = $target.ToLowerInvariant()
                if ($owner.ContainsKey($key)) {
                    Fail "modules '$($owner[$key])' and '$name' both overlay $target"
                }
                $owner[$key] = $name
                $replace[$target] = $source
            }
            if (-not $sawModulesGo) {
                Fail "module $name overlay does not map modules\modules.go (no import to enable it)"
            }
            $included.Add($name)
            continue
        }

        if (Get-ChildItem -Path $dir.FullName -Filter *.go -File) {
            $imp = "gurimoon/modules/$name"
            if (-not $imports.Contains($imp)) { $imports.Add($imp) }
            $included.Add($name)
            Write-Host "module $name : in-tree, imported as $imp"
        }
    }
}

# -- Build ---------------------------------------------------------------------
if ($Mode -eq "check") {
    if (-not (Test-Path $BuildDir)) { New-Item -ItemType Directory $BuildDir | Out-Null }
    $out = Join-Path $BuildDir "gurimoon-check.exe"
} else {
    $out = Join-Path $Root "gurimoon.exe"
}

$goArgs = @("build")
if ($imports.Count -gt 0) {
    if (-not (Test-Path $BuildDir)) { New-Item -ItemType Directory $BuildDir | Out-Null }

    $gen = "// Code generated by rebuild.ps1. DO NOT EDIT.`n" +
           "// Exists only inside the -overlay build; modules\modules.go on disk is untouched.`n" +
           "package modules`n`nimport (`n"
    foreach ($imp in $imports) { $gen += "`t_ `"$imp`"`n" }
    $gen += ")`n"
    $genPath = Join-Path $BuildDir "modules_enabled.go"
    [System.IO.File]::WriteAllText($genPath, $gen)
    $replace[$modulesGo] = $genPath

    $overlayPath = Join-Path $BuildDir "overlay.json"
    $json = @{ Replace = $replace } | ConvertTo-Json -Depth 5
    [System.IO.File]::WriteAllText($overlayPath, $json)
    Write-Host "overlay: $($replace.Count) file(s) mapped -> $overlayPath"

    $goArgs += @("-overlay", $overlayPath)
    Write-Host "=== Building server WITH modules: $($included -join ', ') ==="
} elseif ($Mode -eq "core") {
    Write-Host "=== Building server, core only (no modules) ==="
} else {
    Write-Host "=== Building server (no modules found) ==="
}
if ($tags.Count -gt 0) {
    $goArgs += @("-tags", ($tags -join ","))
    Write-Host "build tags: $($tags -join ',')"
}
$goArgs += @("-o", $out, ".\cmd\gurimoon\")

Push-Location $Root
try {
    & go @goArgs
    if ($LASTEXITCODE -ne 0) { Fail "go build" }
} finally {
    Pop-Location
}

$mb = [math]::Round((Get-Item $out).Length / 1MB, 1)
Write-Host "=== Build OK: $out ($mb MB) ==="
exit 0
