# matrix/hardened: Windows-native tests use private homes and explicitly opt in to owned ConPTY fixtures.
[CmdletBinding()]
param(
    [string]$GoPath='',
    [string[]]$Packages=@(
        './cmd/agent-deck-native',
        './internal/conpty',
        './internal/terminalhost',
        './internal/nativeui',
        './internal/nativefleet',
        './internal/nativeregistry',
        './internal/nativestatestore',
        './internal/nativeterminal',
        './internal/terminalbackend',
        './internal/winipc'
    ),
    [switch]$NativeIntegration
)

$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
Set-StrictMode -Version 2.0

function Assert-PlainPath([string]$Path,[bool]$Directory) {
    $fullPath=[IO.Path]::GetFullPath($Path)
    $cursor=$fullPath
    while($cursor){
        $item=Get-Item -LiteralPath $cursor -Force
        if(($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0){
            throw ('Test path contains a reparse point; refused: '+$cursor)
        }
        if($cursor -eq $fullPath -and $item.PSIsContainer -ne $Directory){throw 'Test path has an unexpected type.'}
        if($cursor -ne $fullPath -and -not $item.PSIsContainer){throw 'Test path has a non-directory parent.'}
        $parent=[IO.Directory]::GetParent($cursor)
        if($null -eq $parent){break}
        $cursor=$parent.FullName
    }
    return $fullPath
}

# matrix/hardened: apply the private ACL at creation, before a test can create files.
function New-PrivateRoot([string]$Path) {
    Assert-PlainPath ([IO.Path]::GetDirectoryName($Path)) $true|Out-Null
    if(Test-Path -LiteralPath $Path){throw 'The GUID test root already exists; it will not be adopted.'}
    $acl=New-Object Security.AccessControl.DirectorySecurity
    $currentSID=[Security.Principal.WindowsIdentity]::GetCurrent().User
    $acl.SetOwner($currentSID)
    $acl.SetAccessRuleProtection($true,$false)
    $inheritance=[Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
    foreach($sid in @($currentSID,(New-Object Security.Principal.SecurityIdentifier('S-1-5-18')))){
        $rule=New-Object Security.AccessControl.FileSystemAccessRule($sid,[Security.AccessControl.FileSystemRights]::FullControl,$inheritance,[Security.AccessControl.PropagationFlags]::None,[Security.AccessControl.AccessControlType]::Allow)
        $acl.AddAccessRule($rule)
    }
    [IO.Directory]::CreateDirectory($Path,$acl)|Out-Null
    Assert-PrivateRoot $Path
}

function Assert-PrivateRoot([string]$Path) {
    $fullPath=Assert-PlainPath $Path $true
    $currentSID=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $acl=Get-Acl -LiteralPath $fullPath
    if($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -ne $currentSID -or -not $acl.AreAccessRulesProtected){
        throw 'The test root owner or protected ACL changed; retained for inspection.'
    }
    $rules=@($acl.GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier]))
    $seen=@()
    foreach($rule in $rules){
        $sid=$rule.IdentityReference.Value
        if($sid -notin @($currentSID,'S-1-5-18') -or $sid -in $seen -or
            $rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow -or
            $rule.FileSystemRights -ne [Security.AccessControl.FileSystemRights]::FullControl -or
            $rule.InheritanceFlags -ne [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit' -or
            $rule.PropagationFlags -ne [Security.AccessControl.PropagationFlags]::None){
            throw 'The test root permissions changed; retained for inspection.'
        }
        $seen+=@($sid)
    }
    if($rules.Count -ne 2){throw 'The test root has unexpected access rules; retained for inspection.'}
    return $fullPath
}

# matrix/hardened: pin the new root against rename/delete and verify its actual
# volume/file identity. No process scans or permission to stop unrelated agents.
if(-not ('AgentDeck.WindowsNativeTests.PinnedRoot' -as [type])){
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;
namespace AgentDeck.WindowsNativeTests {
    public sealed class PinnedRoot : IDisposable {
        [StructLayout(LayoutKind.Sequential)]
        private struct FileInformation {
            public uint Attributes;
            public System.Runtime.InteropServices.ComTypes.FILETIME Created, Accessed, Written;
            public uint Volume, SizeHigh, SizeLow, Links, IndexHigh, IndexLow;
        }
        [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
        private static extern SafeFileHandle CreateFileW(string path, uint access, uint share,
            IntPtr security, uint creation, uint flags, IntPtr template);
        [DllImport("kernel32.dll", SetLastError=true)]
        private static extern bool GetFileInformationByHandle(SafeFileHandle handle, out FileInformation info);
        private SafeFileHandle handle;
        private readonly string path;
        private readonly FileInformation identity;
        private static SafeFileHandle Open(string path) {
            SafeFileHandle result=CreateFileW(path, 0x80, 3, IntPtr.Zero, 3, 0x02200000, IntPtr.Zero);
            if(result.IsInvalid){int error=Marshal.GetLastWin32Error();result.Dispose();throw new Win32Exception(error);}
            return result;
        }
        private static FileInformation Read(SafeFileHandle handle) {
            FileInformation info;
            if(!GetFileInformationByHandle(handle,out info)){throw new Win32Exception(Marshal.GetLastWin32Error());}
            if((info.Attributes & 0x400)!=0 || (info.Attributes & 0x10)==0){
                throw new InvalidOperationException("The test root is no longer a plain directory.");
            }
            return info;
        }
        public PinnedRoot(string path) {
            this.path=path;
            handle=Open(path);
            try {identity=Read(handle);} catch {handle.Dispose();throw;}
        }
        public void AssertIdentity() {
            if(handle==null){throw new ObjectDisposedException("PinnedRoot");}
            using(SafeFileHandle current=Open(path)) {
                FileInformation info=Read(current);
                if(info.Volume!=identity.Volume || info.IndexHigh!=identity.IndexHigh || info.IndexLow!=identity.IndexLow){
                    throw new InvalidOperationException("The test root file identity changed; cleanup refused.");
                }
            }
        }
        public void Dispose(){if(handle!=null){handle.Dispose();handle=null;}}
    }
}
'@
}

# matrix/hardened: pin every traversed directory until its children are removed.
# Parent handles deny rename/delete, closing intermediate-junction replacement
# races. No recursive delete, reparse traversal or unrelated process cleanup.
function Remove-OwnedRoot([string]$Path,$Guard) {
    $Guard.AssertIdentity()
    Assert-PrivateRoot $Path|Out-Null
    $pending=New-Object 'Collections.Generic.Stack[string]'
    $directories=New-Object 'Collections.Generic.List[string]'
    $files=New-Object 'Collections.Generic.List[string]'
    $directoryGuards=New-Object 'Collections.Generic.Dictionary[string,object]'
    $directoryGuards.Add($Path,$Guard)
    $pending.Push($Path)
    try {
        while($pending.Count -gt 0){
            $directory=$pending.Pop()
            $Guard.AssertIdentity()
            $directoryGuards[$directory].AssertIdentity()
            Assert-PlainPath $directory $true|Out-Null
            foreach($entry in [IO.Directory]::GetFileSystemEntries($directory)){
                $item=Get-Item -LiteralPath $entry -Force
                Assert-PlainPath $entry $item.PSIsContainer|Out-Null
                if($item.PSIsContainer){
                    # OPEN_REPARSE_POINT rechecks the object while opening it;
                    # all its ancestors are already pinned before this access.
                    $childGuard=New-Object AgentDeck.WindowsNativeTests.PinnedRoot($entry)
                    $directoryGuards.Add($entry,$childGuard)
                    $directories.Add($entry)
                    $pending.Push($entry)
                } else {$files.Add($entry)}
            }
        }
        foreach($file in $files){
            $Guard.AssertIdentity()
            $directoryGuards[[IO.Path]::GetDirectoryName($file)].AssertIdentity()
            Assert-PlainPath $file $false|Out-Null
            [IO.File]::Delete($file)
        }
        for($index=$directories.Count-1;$index -ge 0;$index--){
            $directory=$directories[$index]
            $Guard.AssertIdentity()
            $directoryGuards[$directory].AssertIdentity()
            Assert-PlainPath $directory $true|Out-Null
            $directoryGuards[$directory].Dispose()
            # The parent stays pinned. Delete(false) cannot traverse a replaced
            # leaf junction, and refuses any directory with new children.
            [IO.Directory]::Delete($directory,$false)
        }
        $Guard.AssertIdentity()
        Assert-PrivateRoot $Path|Out-Null
        $Guard.Dispose()
        [IO.Directory]::Delete($Path,$false)
    } finally {
        foreach($directoryGuard in $directoryGuards.Values){$directoryGuard.Dispose()}
    }
}

$root=$null
$guard=$null
$savedEnvironment=$null
$savedLocation=Get-Location
$exitCode=1
try {
    if([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT){throw 'This runner requires native Windows PowerShell 5.1 or later.'}
    if(-not $GoPath){
        $command=Get-Command go.exe -CommandType Application -ErrorAction Stop|Select-Object -First 1
        $GoPath=$command.Source
    } elseif(-not [IO.Path]::IsPathRooted($GoPath) -or $GoPath -match '^[A-Za-z]:[^\\]'){
        throw 'GoPath must identify an absolute go.exe path.'
    }
    $GoPath=Assert-PlainPath $GoPath $false
    if([IO.Path]::GetFileName($GoPath) -ine 'go.exe'){throw 'GoPath must identify go.exe.'}
    if($null -eq $Packages -or $Packages.Count -eq 0){throw 'At least one local package is required.'}
    foreach($package in $Packages){
        if($package -notmatch '^\./[a-zA-Z0-9_./-]+$' -or $package -match '(?:^|/)\.\.(?:/|$)' -or
            $package -match '/\.(?:/|$)' -or $package -match '//'){
            throw 'Packages must be local ./ package paths without parent traversal or Go flags.'
        }
    }
    $repository=Assert-PlainPath ([IO.Directory]::GetParent($PSScriptRoot).FullName) $true
    Set-Location -LiteralPath $repository
    # Resolve shared Go caches/toolchains before replacing USERPROFILE and APPDATA.
    $cacheJSON=& ([WildcardPattern]::Escape($GoPath)) 'env' '-json' 'GOPATH' 'GOMODCACHE' 'GOCACHE' 'GOTOOLCHAIN'
    if($LASTEXITCODE -ne 0){throw 'go env failed before test isolation.'}
    $cache=($cacheJSON -join "`n")|ConvertFrom-Json
    foreach($name in @('GOPATH','GOMODCACHE','GOCACHE')){
        if(-not $cache.$name -or $cache.$name -eq 'off'){throw ('A persistent Go cache is required: '+$name)}
    }
    $temporaryParent=Assert-PlainPath ([IO.Path]::GetTempPath()) $true
    $root=Join-Path $temporaryParent ('agent-deck-native-tests-'+[Guid]::NewGuid().ToString())
    New-PrivateRoot $root|Out-Null
    $guard=New-Object AgentDeck.WindowsNativeTests.PinnedRoot($root)
    $homeDirectory=Join-Path $root 'home'
    $isolated=@{
        HOME=$homeDirectory;USERPROFILE=$homeDirectory
        APPDATA=(Join-Path $homeDirectory 'AppData\Roaming')
        LOCALAPPDATA=(Join-Path $homeDirectory 'AppData\Local')
        CODEX_HOME=(Join-Path $homeDirectory '.codex')
        XDG_CONFIG_HOME=(Join-Path $root 'config');XDG_DATA_HOME=(Join-Path $root 'data')
        XDG_CACHE_HOME=(Join-Path $root 'cache');XDG_STATE_HOME=(Join-Path $root 'state')
        XDG_RUNTIME_DIR=(Join-Path $root 'run');TMUX_TMPDIR=(Join-Path $root 'tmux')
        TEMP=(Join-Path $root 'tmp');TMP=(Join-Path $root 'tmp');TMPDIR=(Join-Path $root 'tmp')
        GOTMPDIR=(Join-Path $root 'tmp');TZ='UTC';GOENV='off';CGO_ENABLED='0'
        PSModulePath=(Join-Path $PSHOME 'Modules')
    }
    foreach($name in @('HOME','APPDATA','LOCALAPPDATA','CODEX_HOME','XDG_CONFIG_HOME','XDG_DATA_HOME',
            'XDG_CACHE_HOME','XDG_STATE_HOME','XDG_RUNTIME_DIR','TMUX_TMPDIR','TEMP')){
        [IO.Directory]::CreateDirectory($isolated[$name])|Out-Null
    }
    $isolated.HOMEDRIVE=[IO.Path]::GetPathRoot($homeDirectory).TrimEnd('\')
    $isolated.HOMEPATH=$homeDirectory.Substring($isolated.HOMEDRIVE.Length)
    # matrix/hardened: preserve only Windows execution basics and resolved Go
    # caches. API credentials, original config homes and all AGENTDECK flags are
    # absent; finally restores the complete caller process environment verbatim.
    $savedEnvironment=[Environment]::GetEnvironmentVariables('Process')
    foreach($name in @('PATH','SystemRoot','WINDIR','ComSpec','PATHEXT','SystemDrive','ProgramFiles',
            'ProgramFiles(x86)','ProgramW6432','CommonProgramFiles','CommonProgramFiles(x86)',
            'CommonProgramW6432','PROCESSOR_ARCHITECTURE','PROCESSOR_ARCHITEW6432','NUMBER_OF_PROCESSORS',
            'OS','COMPUTERNAME','USERNAME','USERDOMAIN','TERM','LANG','LC_ALL')){
        # .NET Framework's snapshot Hashtable may have case-sensitive keys,
        # while Windows environment lookup (Path/PATH) is case-insensitive.
        $value=[Environment]::GetEnvironmentVariable($name,'Process')
        if($null -ne $value){$isolated[$name]=[string]$value}
    }
    foreach($name in @('GOPATH','GOMODCACHE','GOCACHE','GOTOOLCHAIN')){$isolated[$name]=[string]$cache.$name}
    foreach($name in @($savedEnvironment.Keys)){[Environment]::SetEnvironmentVariable([string]$name,$null,'Process')}
    foreach($name in $isolated.Keys){[Environment]::SetEnvironmentVariable([string]$name,[string]$isolated[$name],'Process')}
    Write-Host ('Isolated native test root: '+$root)
    if($NativeIntegration){
        # matrix/hardened: never inherit an external CLI/launcher or online QA
        # home. Build only this checkout's CLI; real model tests remain disabled.
        $cli=Join-Path $root 'agent-deck-native.exe'
        & ([WildcardPattern]::Escape($GoPath)) 'build' '-o' $cli './cmd/agent-deck-native'
        if($LASTEXITCODE -ne 0){$exitCode=$LASTEXITCODE;throw 'Building the owned native integration CLI failed.'}
        [Environment]::SetEnvironmentVariable('AGENTDECK_NATIVE_TEST','1','Process')
        [Environment]::SetEnvironmentVariable('AGENTDECK_CONPTY_TEST','1','Process')
        [Environment]::SetEnvironmentVariable('AGENTDECK_NATIVE_CLI',$cli,'Process')
    }
    $testArguments=@('test','-count=1','-timeout=6m')+$Packages
    & ([WildcardPattern]::Escape($GoPath)) @testArguments
    $exitCode=$LASTEXITCODE
} catch {
    Write-Warning $_.Exception.Message
    if($exitCode -eq 0){$exitCode=1}
} finally {
    if($null -ne $savedEnvironment){
        foreach($name in @([Environment]::GetEnvironmentVariables('Process').Keys)){
            [Environment]::SetEnvironmentVariable([string]$name,$null,'Process')
        }
        foreach($name in $savedEnvironment.Keys){
            [Environment]::SetEnvironmentVariable([string]$name,[string]$savedEnvironment[$name],'Process')
        }
    }
    Set-Location -LiteralPath $savedLocation.Path
    if($root){
        if($exitCode -eq 0 -and $null -ne $guard){
            try {Remove-OwnedRoot $root $guard;Write-Host 'Isolated native tests passed; owned root removed.'}
            catch {$exitCode=1;Write-Warning ('Cleanup refused or failed; root retained: '+$root+'; '+$_.Exception.Message)}
        } else {Write-Warning ('Isolated native tests failed or setup was incomplete; root retained: '+$root)}
    }
    if($null -ne $guard){$guard.Dispose()}
}
exit $exitCode
