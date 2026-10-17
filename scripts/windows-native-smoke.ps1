# matrix/hardened: explicit native prototype test, with an owned temporary task.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BinaryPath,
    [string]$ReportPath,
    [string]$CodexPath,
    [switch]$StartOnly
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding($false)
$BinaryPath=(Resolve-Path -LiteralPath $BinaryPath).Path
$testId=[Guid]::NewGuid().ToString('N')
$work=Join-Path ([IO.Path]::GetTempPath()) ('agent-deck-native-smoke-'+$testId)
$pipe='\\.\pipe\agent-deck-native-'+$testId
$taskName='AgentDeckNativeSmoke-'+$testId
$taskRegistered=$false
$hostIdentity=$null
$didStop=$false
$cleanupRequested=$true
$result=[ordered]@{test_id=$testId;passed=$false;checks=@();work_directory=$work;task_removed=$false}
New-Item -ItemType Directory -Path $work|Out-Null

function Invoke-Native([string]$operation,[string[]]$arguments=@(),[switch]$Quiet) {
    if($Quiet){$raw=(& $BinaryPath $operation --pipe $pipe --json @arguments 2>$null|Out-String).Trim()}
    else{$raw=(& $BinaryPath $operation --pipe $pipe --json @arguments|Out-String).Trim()}
    if($LASTEXITCODE -ne 0){throw "Native client failed: $operation"}
    $response=$raw|ConvertFrom-Json
    if(-not $response.ok){throw "Native host rejected: $operation"}
    return $response
}
function Wait-Screen([string]$expected) {
    $deadline=[DateTime]::UtcNow.AddSeconds(15)
    do {
        $capture=Invoke-Native 'capture'
        if($capture.screen.Contains($expected)){return $capture}
        Start-Sleep -Milliseconds 100
    } while([DateTime]::UtcNow -lt $deadline)
    throw 'Expected executed output did not arrive in terminal screen'
}
function Send-PowerShell([string]$source) {
    $encoded=[Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($source))
    $line='powershell.exe -NoProfile -NonInteractive -EncodedCommand '+$encoded
    $bytes=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($line+"`r"))
    Invoke-Native 'send' @('--base64',$bytes)|Out-Null
}

try {
    # Task Scheduler supplies an independent host lifetime across SSH clients.
    # The action redirects host diagnostics into this run's dedicated directory.
    $wrapper=Join-Path $work 'host.ps1'
    $quotedBinary=$BinaryPath.Replace("'","''")
    $quotedWork=$work.Replace("'","''")
    $wrapperSource="`$ErrorActionPreference='Stop'`n& '$quotedBinary' host --pipe '$pipe' --cwd '$quotedWork' --cols 120 --rows 40 -- '`$env:COMSPEC' /d /q"
    # Expand only the explicit executable path, not any user-controlled shell text.
    $wrapperSource=$wrapperSource.Replace("'`$env:COMSPEC'",("'"+$env:COMSPEC.Replace("'","''")+"'"))
    $wrapperSource+=" 1> '$quotedWork\host.stdout.log' 2> '$quotedWork\host.stderr.log'`nexit `$LASTEXITCODE`n"
    [IO.File]::WriteAllText($wrapper,$wrapperSource,(New-Object Text.UTF8Encoding($true)))
    $action=New-ScheduledTaskAction -Execute ($env:SystemRoot+'\System32\WindowsPowerShell\v1.0\powershell.exe') -Argument ('-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "'+$wrapper+'"')
    $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $principal=New-ScheduledTaskPrincipal -UserId $sid -LogonType S4U -RunLevel Highest
    $settings=New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 5)
    $definition=New-ScheduledTask -Action $action -Principal $principal -Settings $settings
    Register-ScheduledTask -TaskName $taskName -InputObject $definition|Out-Null
    $taskRegistered=$true
    Start-ScheduledTask -TaskName $taskName
    $deadline=[DateTime]::UtcNow.AddSeconds(15)
    do {
        try {$hostIdentity=Invoke-Native 'status' -Quiet;break} catch {Start-Sleep -Milliseconds 200}
    } while([DateTime]::UtcNow -lt $deadline)
    if(-not $hostIdentity){throw 'Independent native host failed to start'}
    if(-not $hostIdentity.running){throw 'Native shell exited during startup'}
    $result.host_pid=$hostIdentity.host_pid
    $result.child_pid=$hostIdentity.pid
    $result.checks+='independent ConPTY shell started'
    if($StartOnly){
        # Explicit split-connection test: caller must stop/unregister this exact
        # task, or its five-minute execution limit terminates the private host.
        $result.pipe=$pipe
        $result.task_name=$taskName
        $result.passed=$true
        $result.stage='start_only'
        $cleanupRequested=$false
        return
    }

    $marker='NATIVE_'+$testId.Substring(0,12)
    $source="[Console]::OutputEncoding=New-Object Text.UTF8Encoding(`$false);[Console]::WriteLine('$marker')"
    Send-PowerShell $source
    $first=Wait-Screen $marker
    $result.checks+='executed output captured through ConPTY and VT screen'
    $after=$first.head_offset

    Invoke-Native 'resize' @('--cols','132','--rows','42')|Out-Null
    Send-PowerShell "[Console]::WriteLine('SIZE='+[Console]::WindowWidth+'x'+[Console]::WindowHeight)"
    Wait-Screen 'SIZE=132x42'|Out-Null
    $resized=Invoke-Native 'status'
    if($resized.cols -ne 132 -or $resized.rows -ne 42){throw 'Resize metadata disagrees with native console'}
    $result.checks+='native console and host both report 132x42'

    Send-PowerShell "[Console]::OutputEncoding=New-Object Text.UTF8Encoding(`$false);[Console]::WriteLine([string][char]0x4E2D+[string][char]0x6587+[string][char]0x8F93+[string][char]0x51FA)"
    Wait-Screen ([string][char]0x4E2D+[string][char]0x6587+[string][char]0x8F93+[string][char]0x51FA)|Out-Null
    $result.checks+='Chinese output survives native console and screen decoding'

    # Every preceding client exited. A fresh client must find identical ownership.
    Start-Sleep -Milliseconds 500
    $reconnected=Invoke-Native 'status'
    if(-not $reconnected.running -or $reconnected.pid -ne $hostIdentity.pid -or $reconnected.host_pid -ne $hostIdentity.host_pid){throw 'Client disconnect changed host or child identity'}
    $delta=Invoke-Native 'capture' @('--after',([string]$after))
    if($delta.truncated -or $delta.start_offset -ne $after -or $delta.head_offset -le $after){throw 'Output cursor failed across client reconnection'}
    $result.checks+='fresh clients reconnect to unchanged host/child and contiguous output cursor'

    if($CodexPath){
        $CodexPath=(Resolve-Path -LiteralPath $CodexPath).Path
        $quotedCodex=$CodexPath.Replace("'","''")
        Send-PowerShell "& '$quotedCodex' --version"
        $codexVersion=Wait-Screen 'codex-cli'
        $result.checks+='real native Codex executable runs through ConPTY'
        $codexHome=Join-Path $work 'codex-home'
        New-Item -ItemType Directory -Path $codexHome|Out-Null
        $quotedCodexHome=$codexHome.Replace("'","''")
        Send-PowerShell "`$env:CODEX_HOME='$quotedCodexHome'; & '$quotedCodex' --no-alt-screen"
        Wait-Screen 'Codex'|Out-Null
        $result.checks+='Codex startup screen rendered with an isolated CODEX_HOME; no model prompt submitted'
        Invoke-Native 'send' @('--base64','Aw==')|Out-Null
    }

    $stopped=Invoke-Native 'stop'
    $didStop=$true
    if($stopped.running){throw 'Stop acknowledged before process termination'}
    $deadline=[DateTime]::UtcNow.AddSeconds(10)
    do {
        $ownedHost=Get-Process -Id $hostIdentity.host_pid -ErrorAction SilentlyContinue
        $ownedChild=Get-Process -Id $hostIdentity.pid -ErrorAction SilentlyContinue
        if(-not $ownedHost -and -not $ownedChild){break}
        Start-Sleep -Milliseconds 100
    } while([DateTime]::UtcNow -lt $deadline)
    if($ownedHost -or $ownedChild){throw 'Owned host or root remains after stop'}
    $result.checks+='stop waits for owned process termination; host exits'
    $result.passed=$true
} catch {
    $result.error=$_.Exception.Message
} finally {
    if($taskRegistered -and $cleanupRequested){
        if(-not $didStop){try {Invoke-Native 'stop'|Out-Null} catch {}}
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
        $result.task_removed=(-not [bool](Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue))
    }
    if($cleanupRequested -and -not $result.task_removed){$result.passed=$false;$result.cleanup_error='Temporary test task was not removed'}
    $result.completed_utc=[DateTime]::UtcNow.ToString('o')
    $json=$result|ConvertTo-Json -Depth 5
    if($ReportPath){[IO.File]::WriteAllText($ReportPath,$json,(New-Object Text.UTF8Encoding($false)))}
    Write-Output $json
}
if(-not $result.passed){exit 1}
