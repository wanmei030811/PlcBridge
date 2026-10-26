# scripts/fix-utf8.ps1：统一修复 scripts/ 下 PowerShell 脚本的 UTF-8 BOM。
#
# 为什么需要这个脚本：
#
#   Windows PowerShell 5.1（很多现场机器的默认 shell）读取 .ps1 时，
#   若文件是「UTF-8 无 BOM」，会按系统 ANSI 代码页（GBK / 936）解码，
#   导致中文字符变成乱码，甚至让脚本**语法解析失败**
#   （实测：supervisor.ps1 丢失 BOM 后出现 7 个 parse error：
#     "变量引用无效" / "字符串缺少终止符" / "语句块或类型定义中缺少右}"）。
#
#   症状很隐蔽：文件内容看着正常，但 `.\scripts\supervisor.ps1` 直接报错，
#   或者中文字符串（如错误提示）变成乱码。
#
#   PowerShell 7+ (pwsh) 默认按 UTF-8 读取，无 BOM 也没问题；
#   但为了兼容 5.1，所有含非 ASCII 字符的 .ps1 都应带 BOM。
#
# 用法：
#   .\scripts\fix-utf8.ps1           # 修复 scripts/ 下所有 .ps1
#   .\scripts\fix-utf8.ps1 -Path F:\some\dir   # 修复指定目录
#
# 幂等：已经是 BOM 的文件会被跳过。

[CmdletBinding()]
param(
    # 注意：默认值不能在 param 块里用 $PSScriptRoot ——
    # 通过 `powershell -File script.ps1` 启动时该变量在此处尚未赋值（为空），
    # 会导致 Join-Path 报 "empty string" 错。
    # 改在脚本体内解析。
    [string]$Path = ''
)

$ErrorActionPreference = 'Stop'

if (-not $Path) {
    if ($PSScriptRoot) {
        $Path = $PSScriptRoot
    } else {
        $Path = Split-Path -Parent $MyInvocation.MyCommand.Path
    }
}

# 校验每个脚本的 PowerShell 语法（BOM 修好后必须能解析）。
function Test-ParseOk {
    param([string]$File)
    $errors = $null
    $tokens = $null
    [System.Management.Automation.Language.Parser]::ParseFile($File, [ref]$tokens, [ref]$errors) | Out-Null
    if ($errors -and $errors.Count -gt 0) { return $false }
    return $true
}

$files = Get-ChildItem -Path $Path -Filter '*.ps1' -File -Recurse
if (-not $files) {
    Write-Host "[fix-utf8] no .ps1 files under $Path" -ForegroundColor Yellow
    exit 0
}

$fixed = 0
$skipped = 0
foreach ($f in $files) {
    $bytes = [System.IO.File]::ReadAllBytes($f.FullName)
    $hasBom = ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF)

    if ($hasBom) {
        $skipped++
        continue
    }

    # 先确认当前状态确实坏了（能解析就不用动）。
    if (Test-ParseOk -File $f.FullName) {
        # 能解析但没有 BOM：仍然加上 BOM，保证 5.1 下的中文输出正确。
        # （纯 ASCII 脚本加不加 BOM 都行，但统一加更省心。）
    }

    $text = [System.IO.File]::ReadAllText($f.FullName)
    [System.IO.File]::WriteAllText($f.FullName, $text, [System.Text.UTF8Encoding]::new($true))
    $fixed++
    Write-Host "[fix-utf8] added BOM: $($f.Name)" -ForegroundColor Green
}

Write-Host "[fix-utf8] done. fixed=$fixed skipped=$skipped" -ForegroundColor Cyan

# 最终校验：所有脚本必须能解析。
$bad = @()
foreach ($f in $files) {
    if (-not (Test-ParseOk -File $f.FullName)) { $bad += $f.Name }
}
if ($bad.Count -gt 0) {
    Write-Host "[fix-utf8] FAILED to parse: $($bad -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host "[fix-utf8] all scripts parse cleanly" -ForegroundColor Green
