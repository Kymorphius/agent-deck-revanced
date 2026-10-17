# matrix/hardened: launch only the explicitly verified native build, without changing Codex settings.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BinaryPath,
    [Parameter(Mandatory=$true)][ValidatePattern('^[0-9a-fA-F]{64}$')][string]$ExpectedSHA256,
    [Parameter(Mandatory=$true)][ValidatePattern('^[0-9a-fA-F]{64}$')][string]$ExpectedLauncherSHA256,
    [Parameter(Mandatory=$true)][string]$StateDirectory,
    [Parameter(Mandatory=$true)][string]$WorkingDirectory,
    [switch]$Console,
    [ValidatePattern('^([0-9a-fA-F]{64})?$')][string]$ExpectedCodexManifestSHA256=''
)

$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'

function Assert-PlainPath([string]$Path,[bool]$Directory) {
    $fullPath=[IO.Path]::GetFullPath($Path)
    $item=Get-Item -LiteralPath $fullPath -Force
    if($item.PSIsContainer -ne $Directory){throw '启动路径的类型已改变，请重新安装。'}
    $cursor=$fullPath
    while($cursor){
        $ancestor=Get-Item -LiteralPath $cursor -Force
        if(($ancestor.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0){throw '启动路径包含重定向，已停止启动。'}
        $parent=[IO.Directory]::GetParent($cursor)
        if($null -eq $parent){break}
        $cursor=$parent.FullName
    }
    return $fullPath
}

# matrix/hardened: the shortcut authenticates this manifest before JSON is
# interpreted; it binds only an external executable, never auth or model data.
function Read-VerifiedCodexExecutable([string]$Manifest,[string]$Expected) {
    $Manifest=Assert-PlainPath $Manifest $false
    if((Get-Item -LiteralPath $Manifest -Force) -isnot [IO.FileInfo]){throw 'Codex 运行配置不是普通文件。'}
    $inputStream=[IO.File]::Open($Manifest,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::Read)
    try {
        if($inputStream.Length -gt 65536){throw 'Codex 运行配置过大，请重新安装。'}
        $hash=New-Object Security.Cryptography.SHA256Managed
        try {$actual=([BitConverter]::ToString($hash.ComputeHash($inputStream))).Replace('-','').ToLowerInvariant()}
        finally {$hash.Dispose()}
        if($actual -ine $Expected){throw 'Codex 运行配置校验失败，请重新安装。'}
        $inputStream.Position=0
        $reader=New-Object IO.StreamReader -ArgumentList @($inputStream,(New-Object Text.UTF8Encoding($false,$true)),$false,4096,$true)
        try {$config=$reader.ReadToEnd()|ConvertFrom-Json}
        finally {$reader.Dispose()}
    } finally {$inputStream.Dispose()}
    $properties=@($config.PSObject.Properties.Name)
    if($config -isnot [pscustomobject] -or $properties.Count -ne 2 -or
        $properties -cnotcontains 'path' -or $properties -cnotcontains 'sha256' -or
        $config.path -isnot [string] -or $config.sha256 -isnot [string] -or
        $config.sha256 -cnotmatch '^[0-9a-fA-F]{64}$'){
        throw 'Codex 运行配置格式不符合要求，请重新安装。'
    }
    if($config.path -notmatch '^(?:[a-zA-Z]:[\\/]|\\\\[^\\]+\\[^\\]+\\)' -or
        [IO.Path]::GetFileName($config.path) -ine 'codex.exe'){
        throw 'Codex 运行程序必须是绝对路径的 codex.exe。'
    }
    $executable=Assert-PlainPath $config.path $false
    if((Get-Item -LiteralPath $executable -Force) -isnot [IO.FileInfo]){throw 'Codex 运行程序不是普通文件。'}
    if((Get-FileHash -LiteralPath $executable -Algorithm SHA256).Hash -ine $config.sha256){throw 'Codex 运行程序校验失败，请重新安装。'}
    return $executable
}

function ConvertTo-PSLiteral([string]$Value) {return "'"+$Value.Replace("'","''")+"'"}

try {
    $launcher=Assert-PlainPath $PSCommandPath $false
    if((Get-FileHash -LiteralPath $launcher -Algorithm SHA256).Hash -ine $ExpectedLauncherSHA256){
        throw 'Agent Deck 启动脚本校验失败，请重新安装。'
    }
    $BinaryPath=Assert-PlainPath $BinaryPath $false
    $StateDirectory=Assert-PlainPath $StateDirectory $true
    $WorkingDirectory=Assert-PlainPath $WorkingDirectory $true
    if((Get-FileHash -LiteralPath $BinaryPath -Algorithm SHA256).Hash -ine $ExpectedSHA256){
        throw 'Agent Deck 文件校验失败，请重新安装。'
    }

    $codexExecutable=''
    if($ExpectedCodexManifestSHA256){
        $codexExecutable=Read-VerifiedCodexExecutable (Join-Path ([IO.Path]::GetDirectoryName($launcher)) 'codex-runtime.json') $ExpectedCodexManifestSHA256
    }

    # matrix/hardened: only fixed options and Base64 reach wt's command parser;
    # arbitrary paths, spaces, apostrophes and semicolons stay inside PS literals.
    $terminal=Get-Command wt.exe -CommandType Application -ErrorAction SilentlyContinue|Select-Object -First 1
    if(-not $Console -and $terminal){
        # This independent verifier executes in the new terminal before the
        # launcher is loaded, so a changed script cannot validate itself.
        $payload="`$ErrorActionPreference='Stop';if((Get-FileHash -LiteralPath "+(ConvertTo-PSLiteral $launcher)+
            ' -Algorithm SHA256).Hash -ine '+(ConvertTo-PSLiteral $ExpectedLauncherSHA256)+
            "){throw 'Agent Deck 启动脚本校验失败，请重新安装。'};& ([WildcardPattern]::Escape("+(ConvertTo-PSLiteral $launcher)+'))'+
            ' -BinaryPath '+(ConvertTo-PSLiteral $BinaryPath)+
            ' -ExpectedSHA256 '+(ConvertTo-PSLiteral $ExpectedSHA256)+
            ' -ExpectedLauncherSHA256 '+(ConvertTo-PSLiteral $ExpectedLauncherSHA256)+
            ' -StateDirectory '+(ConvertTo-PSLiteral $StateDirectory)+
            ' -WorkingDirectory '+(ConvertTo-PSLiteral $WorkingDirectory)+' -Console'
        if($ExpectedCodexManifestSHA256){$payload+=' -ExpectedCodexManifestSHA256 '+(ConvertTo-PSLiteral $ExpectedCodexManifestSHA256)}
        $encoded=[Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($payload))
        $powerShellPath=Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
        # The child rechecks the binary before starting the UI. No shell profile,
        # global execution-policy change, scheduled task or resident service.
        try {
            & $terminal.Source '-w' 'new' 'new-tab' '--title' 'Agent Deck 原生版' $powerShellPath '-NoLogo' '-NoProfile' '-ExecutionPolicy' 'Bypass' '-EncodedCommand' $encoded
            if($LASTEXITCODE -eq 0){exit 0}
        } catch {
            # A missing/disabled app execution alias should retain the ordinary
            # console fallback instead of blocking the already verified binary.
        }
        Write-Host 'Windows Terminal 未能打开，正在使用普通控制台。'
    }

    [Console]::OutputEncoding=New-Object Text.UTF8Encoding($false)
    $Host.UI.RawUI.WindowTitle='Agent Deck 原生版'
    Set-Location -LiteralPath $WorkingDirectory
    # matrix/hardened: call-operator command lookup treats brackets as patterns.
    $nativeArguments=@('ui','--store','statedb','--tool','codex','--state-dir',$StateDirectory,'--cwd',$WorkingDirectory)
    if($codexExecutable){$nativeArguments+=@('--codex-executable',$codexExecutable)}
    & ([WildcardPattern]::Escape($BinaryPath)) @nativeArguments
    $nativeExitCode=$LASTEXITCODE
    if($nativeExitCode -ne 0){throw 'Agent Deck 未能正常启动或退出，请查看上方提示。'}
    exit 0
} catch {
    Write-Host $_.Exception.Message -ForegroundColor Red
    if(-not [Console]::IsInputRedirected){Read-Host '按 Enter 关闭窗口'|Out-Null}
    exit 1
}
