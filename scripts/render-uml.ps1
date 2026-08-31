<#
.SYNOPSIS
  Renders every .puml source under docs/uml (recursively) to images.

.PARAMETER OutputDir
  Destination folder for rendered images. Defaults to docs/uml/rendered.
  Pass any other path (e.g. another repo's docs folder) to render there instead.

.PARAMETER Format
  Image format: png (default) or svg.
#>
param(
    [string]$OutputDir,
    [string]$Format = "png"
)

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
$PlantUmlJar = Join-Path $RepoRoot "tools\plantuml.jar"
$SourceDir = Join-Path $RepoRoot "docs\uml"

if (-not $OutputDir) {
    $OutputDir = Join-Path $RepoRoot "docs\uml\rendered"
}

if (-not (Test-Path $PlantUmlJar)) {
    Write-Error "plantuml.jar not found at $PlantUmlJar"
    exit 1
}

New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

# Collect sources from every subdirectory except the render output itself.
$files = Get-ChildItem -Path $SourceDir -Filter *.puml -Recurse |
    Where-Object { $_.FullName -notlike "*\rendered\*" } |
    Sort-Object FullName
if ($files.Count -eq 0) {
    Write-Warning "No .puml files found under $SourceDir"
    exit 0
}

& java -jar $PlantUmlJar "-t$Format" -o $OutputDir $files.FullName

Write-Host "Rendered $($files.Count) diagram(s) to $OutputDir"
