# matrix/hardened: explicit per-user native installation; no updates, services, hooks or PATH edits.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BinaryPath,
    [Parameter(Mandatory=$true)][ValidatePattern('^[0-9a-fA-F]{64}$')][string]$ExpectedSHA256,
    [ValidatePattern('^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,63}$')][string]$InstallVersion='lite',
    [string]$CodexExecutable='',
    [ValidatePattern('^([0-9a-fA-F]{64})?$')][string]$ExpectedCodexSHA256='',
    # matrix/hardened: allow parallel releases without adopting an arbitrary
    # state path or overwriting the active release's shortcut entry.
    [ValidatePattern('^(?!.*\.\.)[a-zA-Z0-9][a-zA-Z0-9._+-]{0,63}$')][string]$StateDirectoryName='LiteState',
    [ValidatePattern('^[a-zA-Z0-9._+ -]{0,32}$')][string]$ShortcutSuffix=''
)

$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
$ExpectedSHA256=$ExpectedSHA256.ToLowerInvariant()
$ExpectedCodexSHA256=$ExpectedCodexSHA256.ToLowerInvariant()

function Assert-PlainPath([string]$Path,[bool]$Directory,[bool]$MayBeMissing=$false) {
    $fullPath=[IO.Path]::GetFullPath($Path)
    $cursor=$fullPath
    while($cursor){
        if(Test-Path -LiteralPath $cursor){
            $item=Get-Item -LiteralPath $cursor -Force
            if(($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0){throw '安装路径包含重定向，已停止安装。'}
            if($cursor -eq $fullPath -and $item.PSIsContainer -ne $Directory){throw '安装路径的类型不符合要求。'}
            if($cursor -ne $fullPath -and -not $item.PSIsContainer){throw '安装路径的父路径不是目录。'}
        } elseif($cursor -eq $fullPath -and -not $MayBeMissing){throw '安装所需的文件或目录不存在。'}
        $parent=[IO.Directory]::GetParent($cursor)
        if($null -eq $parent){break}
        $cursor=$parent.FullName
    }
    return $fullPath
}

function New-PrivateDirectory([string]$Path) {
    $fullPath=Assert-PlainPath $Path $true $true
    if(Test-Path -LiteralPath $fullPath){return (Assert-PrivatePath $fullPath $true)}
    # Create only this owned directory, with its ACL applied before any files.
    $acl=New-Object Security.AccessControl.DirectorySecurity
    $acl.SetOwner([Security.Principal.WindowsIdentity]::GetCurrent().User)
    $acl.SetAccessRuleProtection($true,$false)
    $inheritance=[Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
    foreach($sid in @([Security.Principal.WindowsIdentity]::GetCurrent().User,(New-Object Security.Principal.SecurityIdentifier('S-1-5-18')))){
        $rule=New-Object Security.AccessControl.FileSystemAccessRule($sid,[Security.AccessControl.FileSystemRights]::FullControl,$inheritance,[Security.AccessControl.PropagationFlags]::None,[Security.AccessControl.AccessControlType]::Allow)
        $acl.AddAccessRule($rule)
    }
    [IO.Directory]::CreateDirectory($fullPath,$acl)|Out-Null
    return (Assert-PrivatePath $fullPath $true)
}

# matrix/hardened: unknown existing directories are never adopted or repaired.
# A private parent cannot compensate for a broad or differently owned child.
function Assert-PrivatePath([string]$Path,[bool]$Directory) {
    $fullPath=Assert-PlainPath $Path $Directory
    $currentSID=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $acl=Get-Acl -LiteralPath $fullPath
    if($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -ne $currentSID -or -not $acl.AreAccessRulesProtected){
        throw '已有安装路径不是当前用户的私有目录或文件，未改写权限或文件。'
    }
    $rules=@($acl.GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier]))
    if($rules.Count -ne 2){throw '已有安装路径的权限不符合私有安装要求，未修改。'}
    $seen=@()
    foreach($rule in $rules){
        $sid=$rule.IdentityReference.Value
        if($sid -notin @($currentSID,'S-1-5-18') -or $sid -in $seen -or
            $rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow -or
            $rule.FileSystemRights -ne [Security.AccessControl.FileSystemRights]::FullControl -or
            $rule.PropagationFlags -ne [Security.AccessControl.PropagationFlags]::None){
            throw '已有安装路径包含其他权限，未修改。'
        }
        $expectedInheritance=[Security.AccessControl.InheritanceFlags]::None
        if($Directory){$expectedInheritance=[Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'}
        if($rule.InheritanceFlags -ne $expectedInheritance){throw '已有安装路径的权限继承设置不符合要求，未修改。'}
        $seen+=@($sid)
    }
    return $fullPath
}

function New-PrivateFileACL {
    $acl=New-Object Security.AccessControl.FileSecurity
    $currentSID=[Security.Principal.WindowsIdentity]::GetCurrent().User
    $acl.SetOwner($currentSID)
    $acl.SetAccessRuleProtection($true,$false)
    foreach($sid in @($currentSID,(New-Object Security.Principal.SecurityIdentifier('S-1-5-18')))){
        $rule=New-Object Security.AccessControl.FileSystemAccessRule($sid,[Security.AccessControl.FileSystemRights]::FullControl,[Security.AccessControl.AccessControlType]::Allow)
        $acl.AddAccessRule($rule)
    }
    return $acl
}

function Get-StreamHash($Stream) {
    $hash=New-Object Security.Cryptography.SHA256Managed
    try {return ([BitConverter]::ToString($hash.ComputeHash($Stream))).Replace('-','').ToLowerInvariant()}
    finally {$hash.Dispose()}
}

function Copy-VerifiedFile([string]$Source,[string]$Destination,[string]$Expected) {
    $Source=Assert-PlainPath $Source $false
    $Destination=Assert-PlainPath $Destination $false $true
    Assert-PrivatePath ([IO.Path]::GetDirectoryName($Destination)) $true|Out-Null
    $inputStream=[IO.File]::Open($Source,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::Read)
    try {
        if((Get-StreamHash $inputStream) -ne $Expected){throw '安装文件校验失败，未复制文件。'}
        if(Test-Path -LiteralPath $Destination){
            Assert-PrivatePath $Destination $false|Out-Null
            if((Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash -ine $Expected){throw '版本目录已有不同内容，未覆盖已有文件。'}
            return
        }
        $inputStream.Position=0
        # CreateNew fails if another installer creates the target; never truncate
        # an existing binary or modify the file loaded by an active session.
        $fileACL=New-PrivateFileACL
        $outputStream=New-Object IO.FileStream -ArgumentList @($Destination,[IO.FileMode]::CreateNew,[Security.AccessControl.FileSystemRights]::Write,[IO.FileShare]::None,4096,[IO.FileOptions]::None,$fileACL)
        try {$inputStream.CopyTo($outputStream);$outputStream.Flush($true)}
        finally {$outputStream.Dispose()}
    } finally {$inputStream.Dispose()}
    $Destination=Assert-PrivatePath $Destination $false
    if((Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash -ine $Expected){throw '复制后的文件校验失败，已停止安装。'}
}

# matrix/hardened: an explicitly chosen Codex stays external; never replace the
# npm installation, mutate PATH or copy authentication into the release folder.
function Assert-CodexExecutable([string]$Path,[string]$Expected) {
    if($Path -notmatch '^(?:[a-zA-Z]:[\\/]|\\\\[^\\]+\\[^\\]+\\)' -or
        [IO.Path]::GetFileName($Path) -ine 'codex.exe'){
        throw 'Codex 运行程序必须是绝对路径的 codex.exe。'
    }
    $fullPath=Assert-PlainPath $Path $false
    if((Get-Item -LiteralPath $fullPath -Force) -isnot [IO.FileInfo]){throw 'Codex 运行程序不是普通文件。'}
    if((Get-FileHash -LiteralPath $fullPath -Algorithm SHA256).Hash -ine $Expected){throw 'Codex 运行程序校验失败，未安装。'}
    return $fullPath
}

function Write-VerifiedPrivateFile([byte[]]$Bytes,[string]$Destination,[string]$Expected) {
    $Destination=Assert-PlainPath $Destination $false $true
    Assert-PrivatePath ([IO.Path]::GetDirectoryName($Destination)) $true|Out-Null
    if(Test-Path -LiteralPath $Destination){
        Assert-PrivatePath $Destination $false|Out-Null
        if((Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash -ine $Expected){throw '版本目录已有不同的 Codex 运行配置，未覆盖已有文件。'}
        return
    }
    # The manifest receives its protected owner-only ACL at CreateNew, before
    # any bytes exist. A concurrent installer cannot replace this release data.
    $fileACL=New-PrivateFileACL
    $outputStream=New-Object IO.FileStream -ArgumentList @($Destination,[IO.FileMode]::CreateNew,[Security.AccessControl.FileSystemRights]::Write,[IO.FileShare]::None,4096,[IO.FileOptions]::None,$fileACL)
    try {$outputStream.Write($Bytes,0,$Bytes.Length);$outputStream.Flush($true)}
    finally {$outputStream.Dispose()}
    Assert-PrivatePath $Destination $false|Out-Null
    if((Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash -ine $Expected){throw 'Codex 运行配置写入后的校验失败，已停止安装。'}
}

function ConvertTo-PSLiteral([string]$Value) {return "'"+$Value.Replace("'","''")+"'"}

# matrix/hardened: WScript.Shell truncates shortcut arguments after 1023 UTF-16
# characters. Keep each path/hash once and use a quoted native -Command value.
function New-ShortcutArguments([string]$BaseDirectory,[string]$VersionDirectoryName,[string]$BinaryHash,[string]$LauncherHash,[string]$CodexManifestHash,[ValidatePattern('^(?!.*\.\.)[a-zA-Z0-9][a-zA-Z0-9._+-]{0,63}$')][string]$StateDirectoryName='LiteState') {
    $payload="`$ErrorActionPreference='Stop';`$ad="+(ConvertTo-PSLiteral $BaseDirectory)+
        ';$p=Join-Path $ad '+(ConvertTo-PSLiteral $VersionDirectoryName)+
        ";`$l=Join-Path `$p 'windows-native-launch.ps1';`$h="+(ConvertTo-PSLiteral $LauncherHash)+
        ";if((Get-FileHash -LiteralPath `$l -Algorithm SHA256).Hash -ine `$h){throw 'Agent Deck 启动脚本校验失败，请重新安装。'};& ([WildcardPattern]::Escape(`$l)) -BinaryPath (Join-Path `$p 'agent-deck-native.exe') -ExpectedSHA256 "+(ConvertTo-PSLiteral $BinaryHash)+
        ' -ExpectedLauncherSHA256 $h -StateDirectory (Join-Path $ad '+(ConvertTo-PSLiteral $StateDirectoryName)+
        ") -WorkingDirectory (Join-Path `$ad 'Workspace')"
    if($CodexManifestHash){$payload+=' -ExpectedCodexManifestSHA256 '+(ConvertTo-PSLiteral $CodexManifestHash)}
    # Standard Windows argv quoting preserves the complete PS source as one
    # argument, without cmd.exe. Paths themselves remain PS single literals.
    $quoted=[regex]::Replace($payload,'(\\*)"','$1$1\"')
    $quoted=[regex]::Replace($quoted,'(\\+)$','$1$1')
    $arguments='-NoLogo -NoProfile -ExecutionPolicy Bypass -Command "'+$quoted+'"'
    if($arguments.Length -gt 1023){throw '安装路径过长，Windows 无法完整保存启动入口；未创建快捷方式。'}
    return $arguments
}

function Assert-Shortcut($Shell,[string]$Path,[string]$Target,[string]$Arguments,[string]$Working) {
    $Path=Assert-PlainPath $Path $false $true
    if(-not (Test-Path -LiteralPath $Path)){return $false}
    $shortcut=$Shell.CreateShortcut($Path)
    try {
        if($shortcut.TargetPath -ine $Target -or $shortcut.Arguments -cne $Arguments -or $shortcut.WorkingDirectory -ine $Working){
            throw '桌面或开始菜单已有不同的 Agent Deck 原生版入口，未覆盖；请保留或重命名该入口后再安装。'
        }
    } finally {[Runtime.InteropServices.Marshal]::FinalReleaseComObject($shortcut)|Out-Null}
    return $true
}

function Write-Shortcut($Shell,[string]$Path,[string]$Target,[string]$Arguments,[string]$Working,[string]$Icon) {
    if(Assert-Shortcut $Shell $Path $Target $Arguments $Working){return}
    $temporary=Join-Path ([IO.Path]::GetDirectoryName($Path)) ('AgentDeckNative-install-'+[Guid]::NewGuid().ToString('N')+'.lnk')
    $shortcut=$Shell.CreateShortcut($temporary)
    try {
        $shortcut.TargetPath=$Target
        $shortcut.Arguments=$Arguments
        $shortcut.WorkingDirectory=$Working
        $shortcut.IconLocation=$Icon+',0'
        $shortcut.Description='Agent Deck 原生版：创建 Codex 会话，返回列表并继续会话。'
        $shortcut.WindowStyle=1
        $shortcut.Save()
    } finally {[Runtime.InteropServices.Marshal]::FinalReleaseComObject($shortcut)|Out-Null}
    Assert-Shortcut $Shell $temporary $Target $Arguments $Working|Out-Null
    # An atomic move fails on a newly appeared destination, preserving it.
    [IO.File]::Move($temporary,$Path)
    Assert-Shortcut $Shell $Path $Target $Arguments $Working|Out-Null
}

$shell=$null
try {
    if([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT){throw '该安装器只能在 Windows 上运行。'}
    # matrix/hardened: selecting a newer native Codex is explicit and hash-bound.
    if([bool]$CodexExecutable -ne [bool]$ExpectedCodexSHA256){throw 'CodexExecutable 和 ExpectedCodexSHA256 必须同时提供。'}
    $codexManifestBytes=$null
    $codexManifestHash=''
    if($CodexExecutable){
        $CodexExecutable=Assert-CodexExecutable $CodexExecutable $ExpectedCodexSHA256
        $codexManifestJSON=[ordered]@{path=$CodexExecutable;sha256=$ExpectedCodexSHA256}|ConvertTo-Json -Compress
        $codexManifestBytes=(New-Object Text.UTF8Encoding($false)).GetBytes($codexManifestJSON)
        $manifestStream=New-Object IO.MemoryStream -ArgumentList @(,$codexManifestBytes)
        try {$codexManifestHash=Get-StreamHash $manifestStream}
        finally {$manifestStream.Dispose()}
    }
    $BinaryPath=Assert-PlainPath $BinaryPath $false
    if((Get-FileHash -LiteralPath $BinaryPath -Algorithm SHA256).Hash -ine $ExpectedSHA256){throw 'Agent Deck 文件校验失败，未安装。'}
    $launcherSource=Assert-PlainPath (Join-Path $PSScriptRoot 'windows-native-launch.ps1') $false
    $launcherHash=(Get-FileHash -LiteralPath $launcherSource -Algorithm SHA256).Hash.ToLowerInvariant()
    $baseDirectory=Assert-PlainPath (Join-Path $env:LOCALAPPDATA 'AgentDeckNativeLite') $true $true
    $installDirectory=Join-Path $baseDirectory ($InstallVersion+'-'+$ExpectedSHA256.Substring(0,12))
    $installedBinary=Join-Path $installDirectory 'agent-deck-native.exe'
    $installedLauncher=Join-Path $installDirectory 'windows-native-launch.ps1'
    $codexManifestPath=Join-Path $installDirectory 'codex-runtime.json'
    $stateDirectory=Join-Path $baseDirectory $StateDirectoryName
    $workingDirectory=Join-Path $baseDirectory 'Workspace'
    # Preflight every existing managed boundary before copying files or saving
    # shortcuts. Old releases and an unowned directory retain their bytes/ACLs.
    foreach($directory in @($baseDirectory,$installDirectory,$stateDirectory,$workingDirectory)){
        Assert-PlainPath $directory $true $true|Out-Null
        if(Test-Path -LiteralPath $directory){Assert-PrivatePath $directory $true|Out-Null}
    }
    foreach($entry in @(@($installedBinary,$ExpectedSHA256),@($installedLauncher,$launcherHash))){
        Assert-PlainPath $entry[0] $false $true|Out-Null
        if(Test-Path -LiteralPath $entry[0]){
            Assert-PrivatePath $entry[0] $false|Out-Null
            if((Get-FileHash -LiteralPath $entry[0] -Algorithm SHA256).Hash -ine $entry[1]){throw '已有版本文件内容不同，未覆盖或安装。'}
        }
    }

    # A pre-existing manifest is part of this immutable release, including when
    # a caller omits Codex selection; never silently ignore or adopt that file.
    Assert-PlainPath $codexManifestPath $false $true|Out-Null
    if(Test-Path -LiteralPath $codexManifestPath){
        Assert-PrivatePath $codexManifestPath $false|Out-Null
        if(-not $codexManifestHash -or (Get-FileHash -LiteralPath $codexManifestPath -Algorithm SHA256).Hash -ine $codexManifestHash){
            throw '版本目录已有不同的 Codex 运行配置，未覆盖或安装。'
        }
    }

    $powerShellPath=Assert-PlainPath (Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe') $false
    # The .lnk contains fixed host options and one Windows-quoted PS command.
    # User paths stay in PS literals and are never interpreted by cmd.exe.
    # The verifier lives in the shortcut, outside the script it authenticates.
    # A changed launcher is rejected before its first instruction can execute.
    $arguments=New-ShortcutArguments $baseDirectory ([IO.Path]::GetFileName($installDirectory)) $ExpectedSHA256 $launcherHash $codexManifestHash $StateDirectoryName
    $desktop=[Environment]::GetFolderPath([Environment+SpecialFolder]::DesktopDirectory)
    $programs=[Environment]::GetFolderPath([Environment+SpecialFolder]::Programs)
    if(-not $desktop -or -not $programs){throw '未找到当前用户的桌面或开始菜单目录。'}
    foreach($directory in @($desktop,$programs)){Assert-PlainPath $directory $true|Out-Null}
    $shortcutName='Agent Deck 原生版'
    if($ShortcutSuffix){$shortcutName+=' '+$ShortcutSuffix}
    $shortcutName+='.lnk'
    $shortcuts=@((Join-Path $desktop $shortcutName),(Join-Path $programs $shortcutName))
    $shell=New-Object -ComObject WScript.Shell
    # Check both destinations before copying files or changing either shortcut.
    foreach($shortcutPath in $shortcuts){Assert-Shortcut $shell $shortcutPath $powerShellPath $arguments $workingDirectory|Out-Null}

    New-PrivateDirectory $baseDirectory|Out-Null
    foreach($directory in @($installDirectory,$stateDirectory,$workingDirectory)){New-PrivateDirectory $directory|Out-Null}
    Copy-VerifiedFile $BinaryPath $installedBinary $ExpectedSHA256
    Copy-VerifiedFile $launcherSource $installedLauncher $launcherHash
    if($codexManifestHash){Write-VerifiedPrivateFile $codexManifestBytes $codexManifestPath $codexManifestHash}
    foreach($shortcutPath in $shortcuts){Write-Shortcut $shell $shortcutPath $powerShellPath $arguments $workingDirectory $installedBinary}
    [ordered]@{
        installed=$true;binary_path=$installedBinary;sha256=$ExpectedSHA256
        launcher_path=$installedLauncher;launcher_sha256=$launcherHash;state_directory=$stateDirectory
        working_directory=$workingDirectory;shortcuts=$shortcuts
        existing_files_preserved=$true
        codex_manifest_path=$(if($codexManifestHash){$codexManifestPath}else{''})
        codex_manifest_sha256=$codexManifestHash;codex_executable=$CodexExecutable;codex_sha256=$ExpectedCodexSHA256
    }|ConvertTo-Json -Depth 3
} finally {
    if($shell){[Runtime.InteropServices.Marshal]::FinalReleaseComObject($shell)|Out-Null}
}
