#Requires -Version 7.0
<#
stats.ps1 — 网关请求统计的终端视图（按模型）

数据源是网关的 GET /v1/stats（本仓库 internal/metrics 采集）。
无状态、用完即退：不像常驻面板那样占内存，也不引入任何额外依赖。

用法:
  pwsh -File ./stats.ps1                 # 一次性快照
  pwsh -File ./stats.ps1 -Watch 5s        # 持续刷新（Ctrl+C 退出）
  pwsh -File ./stats.ps1 -Json            # 原始 JSON（供脚本消费）
  pwsh -File ./stats.ps1 -Sort ttfb       # 按首字延迟排序：requests|ttfb|tokens|credit
  pwsh -File ./stats.ps1 -Width 86        # 按指定列宽排版（默认自动探测终端）

环境变量（与其它工具口径一致）:
  WB2A_URL      直接指定网关地址，如 http://127.0.0.1:7863（设置后忽略 config.json）
  WB2A_API_KEY  配合 WB2A_URL 使用的密钥
  WB2A_CONFIG   配置文件路径（默认 ./config.json）

排版说明：默认按终端窗口尺寸自适应 —— 放不下时依次淘汰次要列、压缩模型列、
折叠多余行；窗口窄于最简表则转为滚动输出（不再原地覆盖，避免错位堆叠）。
#>
[CmdletBinding()]
param(
    [string]$Watch = "",
    [switch]$Json,
    [ValidateSet('requests', 'ttfb', 'tokens', 'credit')]
    [string]$Sort = 'requests',
    [int]$Width = 0,
    [int]$Height = 0
)

$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

$exe = Join-Path $PSScriptRoot 'bin\stats.exe'
if (-not (Test-Path $exe)) {
    Write-Host '未找到 bin\stats.exe，正在编译…'
    New-Item -ItemType Directory -Force -Path (Join-Path $PSScriptRoot 'bin') | Out-Null
    & go build -o bin/stats.exe ./cmd/stats
    if ($LASTEXITCODE -ne 0) { Write-Host '编译失败' -ForegroundColor Red; exit 1 }
}

$argv = @()
if ($Json)       { $argv += '-json' }
$argv += @('-sort', $Sort)
if ($Width -gt 0)  { $argv += @('-width',  $Width) }
if ($Height -gt 0) { $argv += @('-height', $Height) }
if ($Watch -ne '') { $argv += @('-watch', $Watch) }

& $exe @argv
exit $LASTEXITCODE
