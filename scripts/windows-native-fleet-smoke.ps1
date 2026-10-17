# matrix/hardened: explicit multi-session native test with GUID-scoped state and bounded, trigger-free tasks.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BinaryPath,
    [string]$ReportPath,
    [switch]$StartOnly
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding($false)
$BinaryPath=(Resolve-Path -LiteralPath $BinaryPath).Path
$testId=[Guid]::NewGuid().ToString('N')
$work=Join-Path ([IO.Path]::GetTempPath()) ('agent-deck-native-fleet-smoke-'+$testId)
$stateDir=Join-Path $work 'registry'
$pipe='\\.\pipe\agent-deck-native-fleet-'+$testId
$taskPrefix='AgentDeckNativeFleetSmoke-'+$testId
$taskNames=@()
$activeHostPID=0
$ownedProcessIds=@()
$cleanupRequested=$true
$result=[ordered]@{
    test_id=$testId;passed=$false;checks=@();work_directory=$work
    registry=$stateDir;pipe=$pipe;task_removed=$false;work_retained=$true
}
New-Item -ItemType Directory -Path $work|Out-Null
# Protect this newly created root before wrappers, project fixtures or logs.
# All ACEs apply only to the GUID directory and inherit to its own children.
$currentSID=[Security.Principal.WindowsIdentity]::GetCurrent().User
$directoryACL=New-Object Security.AccessControl.DirectorySecurity
$directoryACL.SetAccessRuleProtection($true,$false)
$inheritance=[Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
foreach($principalSID in @($currentSID,(New-Object Security.Principal.SecurityIdentifier('S-1-5-18')))){
    $rule=New-Object Security.AccessControl.FileSystemAccessRule($principalSID,[Security.AccessControl.FileSystemRights]::FullControl,$inheritance,[Security.AccessControl.PropagationFlags]::None,[Security.AccessControl.AccessControlType]::Allow)
    $directoryACL.AddAccessRule($rule)
}
Set-Acl -LiteralPath $work -AclObject $directoryACL
$protectedACL=Get-Acl -LiteralPath $work
if(-not $protectedACL.AreAccessRulesProtected -or @($protectedACL.Access).Count -ne 2){throw 'Private test root ACL readback failed'}

function Invoke-Native([string]$operation,[string[]]$arguments=@(),[switch]$ExpectFailure) {
    $nativeArguments=@($operation,'--pipe',$pipe,'--json')
    if($operation -in @('list','create','start','rename','remove','shutdown')){
        $nativeArguments+=@('--state-dir',$stateDir)
    }
    $nativeArguments+=$arguments
    $errorLog=Join-Path $work 'client.stderr.log'
    # Windows PowerShell can convert native stderr into ErrorRecords. Keep
    # expected stale-generation rejection inspectable without swallowing the
    # native exit code, and keep diagnostics inside this private test root.
    $previousPreference=$ErrorActionPreference
    try {
        $ErrorActionPreference='Continue'
        $raw=(& $BinaryPath @nativeArguments 2>$errorLog|Out-String).Trim()
        $nativeExitCode=$LASTEXITCODE
    } finally {$ErrorActionPreference=$previousPreference}
    if($ExpectFailure){
        if($nativeExitCode -eq 0){throw "Native client unexpectedly accepted: $operation"}
        $message=[IO.File]::ReadAllText($errorLog)
        if(-not $message.Contains('native session run changed')){throw 'Old run was rejected for a reason other than generation mismatch'}
        return
    }
    if($nativeExitCode -ne 0){throw "Native client failed: $operation; see client.stderr.log"}
    if(-not $raw){throw "Native client returned no JSON: $operation"}
    $response=$raw|ConvertFrom-Json
    if(-not $response.ok){throw "Native manager rejected: $operation"}
    return $response
}

function Start-IndependentManager([string]$stage) {
    $taskName=$taskPrefix+'-'+$stage
    $wrapper=Join-Path $work ('manager-'+$stage+'.ps1')
    $quotedBinary=$BinaryPath.Replace("'","''")
    $quotedState=$stateDir.Replace("'","''")
    $stdout=(Join-Path $work ('manager-'+$stage+'.stdout.log')).Replace("'","''")
    $stderr=(Join-Path $work ('manager-'+$stage+'.stderr.log')).Replace("'","''")
    $wrapperSource="`$ErrorActionPreference='Stop'`n& '$quotedBinary' serve --pipe '$pipe' --state-dir '$quotedState' 1> '$stdout' 2> '$stderr'`nexit `$LASTEXITCODE`n"
    [IO.File]::WriteAllText($wrapper,$wrapperSource,(New-Object Text.UTF8Encoding($true)))
    $powerShellPath=Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $action=New-ScheduledTaskAction -Execute $powerShellPath -Argument ('-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "'+$wrapper+'"')
    $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $principal=New-ScheduledTaskPrincipal -UserId $sid -LogonType S4U -RunLevel Highest
    $settings=New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 10)
    # No trigger and no service installation: only this explicit test starts it.
    $definition=New-ScheduledTask -Action $action -Principal $principal -Settings $settings
    Register-ScheduledTask -TaskName $taskName -TaskPath '\' -InputObject $definition|Out-Null
    $script:taskNames+=@($taskName)
    $registered=Get-ScheduledTask -TaskName $taskName -TaskPath '\'
    if(@($registered.Triggers|Where-Object {$_}).Count -ne 0 -or $registered.Principal.LogonType -ne 'S4U' -or [System.Xml.XmlConvert]::ToTimeSpan($registered.Settings.ExecutionTimeLimit).TotalMinutes -ne 10){throw 'Registered test task changed its trigger-free S4U or ten-minute bound'}
    Start-ScheduledTask -TaskName $taskName -TaskPath '\'
    $deadline=[DateTime]::UtcNow.AddSeconds(20)
    $manager=$null
    do {
        try {$manager=Invoke-Native 'list';break} catch {Start-Sleep -Milliseconds 200}
    } while([DateTime]::UtcNow -lt $deadline)
    if(-not $manager -or $manager.host_pid -le 0){throw 'Independent native manager failed to start; see manager diagnostics'}
    $script:activeHostPID=[int]$manager.host_pid
    $script:ownedProcessIds+=@([int]$manager.host_pid)
    return [pscustomobject]@{task_name=$taskName;host_pid=[int]$manager.host_pid;sessions=@($manager.sessions|Where-Object {$null -ne $_})}
}

function Remove-OwnedTask([string]$taskName) {
    if($taskName -notin $taskNames){throw 'Refusing to remove a task not registered by this run'}
    $task=Get-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction SilentlyContinue
    if($task){
        Stop-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -TaskPath '\' -Confirm:$false -ErrorAction Stop
    }
    if(Get-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction SilentlyContinue){throw 'Exact temporary test task was not removed'}
}

function Wait-ProcessesGone([int[]]$processIds) {
    $deadline=[DateTime]::UtcNow.AddSeconds(10)
    do {
        $remaining=@($processIds|Where-Object {$_ -gt 0 -and (Get-Process -Id $_ -ErrorAction SilentlyContinue)})
        if($remaining.Count -eq 0){return}
        Start-Sleep -Milliseconds 100
    } while([DateTime]::UtcNow -lt $deadline)
    throw 'Owned native manager or root process remains after acknowledged cleanup'
}

function Assert-Identity($response,$session,[int]$hostPID) {
    if(-not $response.running -or $response.session_id -ne $session.id -or $response.run_id -ne $session.run_id -or $response.pid -ne $session.pid -or $response.host_pid -ne $hostPID){throw 'Session runtime identity or liveness changed unexpectedly'}
}

function Session-Arguments($session) {return @('--session',[string]$session.id,'--run-id',[string]$session.run_id)}

function Send-PowerShell($session,[string]$source,[string]$marker,[switch]$Stale) {
    $encoded=[Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($source))
    $line='powershell.exe -NoProfile -NonInteractive -EncodedCommand '+$encoded
    if($marker -and $line.Contains($marker)){throw 'Output marker appeared in the typed command'}
    $data=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($line+"`r"))
    $arguments=(Session-Arguments $session)+@('--base64',$data)
    if($Stale){Invoke-Native 'send' $arguments -ExpectFailure|Out-Null}
    else {Invoke-Native 'send' $arguments|Out-Null}
}

function Send-Marker($session,[string]$marker) {
    Send-PowerShell $session ("[Console]::OutputEncoding=New-Object Text.UTF8Encoding(`$false);[Console]::WriteLine('$marker')") $marker
}

function Wait-Screen($session,[string]$expected) {
    $deadline=[DateTime]::UtcNow.AddSeconds(20)
    do {
        $capture=Invoke-Native 'capture' (Session-Arguments $session)
        Assert-Identity $capture $session $activeHostPID
        if($capture.screen.Contains($expected)){return $capture}
        Start-Sleep -Milliseconds 100
    } while([DateTime]::UtcNow -lt $deadline)
    throw 'Expected independently executed output did not arrive in the requested session'
}

function Fixture-Session($response,[string]$name,[string]$directory,[string]$marker) {
    if(-not $response.running -or $response.pid -le 0 -or -not $response.session_id -or -not $response.run_id -or $response.host_pid -ne $activeHostPID){throw 'Created shell lacks a live exact session/run/process identity'}
    $script:ownedProcessIds+=@([int]$response.pid)
    return [pscustomobject]@{id=[string]$response.session_id;run_id=[string]$response.run_id;pid=[int]$response.pid;name=$name;dir=$directory;marker=$marker}
}

try {
    $projectOne=Join-Path $work 'project-one'
    $projectTwo=Join-Path $work 'project-two'
    New-Item -ItemType Directory -Path $projectOne,$projectTwo|Out-Null
    $sentinel=Join-Path $projectOne 'preserve-project.txt'
    $sentinelText='Owned project sentinel '+$testId
    [IO.File]::WriteAllText($sentinel,$sentinelText,(New-Object Text.UTF8Encoding($false)))
    $manager=Start-IndependentManager 'initial'
    if($manager.sessions.Count -ne 0){throw 'Fresh private registry contains unexpected descriptors'}
    $result.host_pid=$manager.host_pid
    $result.task_name=$manager.task_name
    $result.checks+='independent manager started under current SID with ten-minute trigger-free task'

    $markerOne='FLEET_ONE_'+$testId
    $markerTwo='FLEET_TWO_'+$testId
    $oneResponse=Invoke-Native 'create' @('--name','shell-one','--tool','shell','--cwd',$projectOne,'--cols','120','--rows','40')
    $one=Fixture-Session $oneResponse 'shell-one' $projectOne $markerOne
    $twoResponse=Invoke-Native 'create' @('--name','shell-two','--tool','shell','--cwd',$projectTwo,'--cols','120','--rows','40')
    $two=Fixture-Session $twoResponse 'shell-two' $projectTwo $markerTwo
    if($one.id -eq $two.id -or $one.run_id -eq $two.run_id -or $one.pid -eq $two.pid){throw 'Two shells share a session, generation or root process'}
    Send-Marker $one $markerOne
    Send-Marker $two $markerTwo
    $screenOne=Wait-Screen $one $markerOne
    $screenTwo=Wait-Screen $two $markerTwo
    if($screenOne.screen.Contains($markerTwo) -or $screenTwo.screen.Contains($markerOne)){throw 'Output leaked across native sessions'}
    $result.initial_sessions=@($one,$two)
    $result.checks+='two real Shell sessions retain distinct IDs, run IDs, PIDs and independently executed output'

    if($StartOnly){
        # The caller owns exact shutdown/unregistration after a separate SSH
        # connection tests this fixture. The task still has a ten-minute limit.
        $result.sessions=@($one,$two)
        $result.session_ids=@($one.id,$two.id)
        $result.run_ids=@($one.run_id,$two.run_id)
        $result.pids=@($one.pid,$two.pid)
        $result.execution_limit_minutes=10
        $result.stage='start_only'
        $result.passed=$true
        $cleanupRequested=$false
        return
    }

    $stopped=Invoke-Native 'stop' (Session-Arguments $one)
    if($stopped.running -or $stopped.session_id -ne $one.id -or $stopped.run_id -ne $one.run_id){throw 'Stop did not acknowledge the exact old run after termination'}
    Wait-ProcessesGone @($one.pid)
    Assert-Identity (Invoke-Native 'status' (Session-Arguments $two)) $two $activeHostPID
    $result.checks+='stopping one shell terminates its root while the other exact run stays live'

    $oldOne=$one
    $newResponse=Invoke-Native 'start' (Session-Arguments $oldOne)
    $one=Fixture-Session $newResponse 'shell-one' $projectOne ('FLEET_RESTART_'+$testId)
    if($one.id -ne $oldOne.id -or $one.run_id -eq $oldOne.run_id -or $one.pid -eq $oldOne.pid){throw 'Explicit restart did not retain session ID and replace run ID/PID'}
    $staleMarker='FLEET_STALE_'+$testId
    Send-PowerShell $oldOne ("[Console]::WriteLine('$staleMarker')") $staleMarker -Stale
    Send-Marker $one $one.marker
    $restartedScreen=Wait-Screen $one $one.marker
    if($restartedScreen.screen.Contains($staleMarker) -or $restartedScreen.screen.Contains($markerOne)){throw 'Restart accepted stale input or retained old terminal output'}
    Assert-Identity (Invoke-Native 'status' (Session-Arguments $two)) $two $activeHostPID
    $result.checks+='restart replaces process generation; old run send is rejected and cannot reach the replacement'

    Invoke-Native 'rename' ((Session-Arguments $one)+@('--name','shell-one-renamed'))|Out-Null
    $one.name='shell-one-renamed'
    # An additional stopped description isolates metadata removal from either
    # retained real shell, allowing both to prove stopped recovery below.
    $temporary=Invoke-Native 'create' @('--name','description-only','--tool','shell','--cwd',$projectOne,'--no-start')
    if($temporary.running -or $temporary.pid -ne 0 -or $temporary.run_id){throw 'No-start descriptor unexpectedly launched a process'}
    Invoke-Native 'rename' @('--session',$temporary.session_id,'--name','description-renamed')|Out-Null
    Invoke-Native 'remove' @('--session',$temporary.session_id)|Out-Null
    $list=Invoke-Native 'list'
    $renamed=@($list.sessions|Where-Object {$_.id -eq $one.id})
    if(@($list.sessions).Count -ne 2 -or $renamed.Count -ne 1 -or $renamed[0].name -ne $one.name -or $renamed[0].dir -ne $projectOne -or @($list.sessions|Where-Object {$_.id -eq $temporary.session_id}).Count -ne 0){throw 'Rename/removal changed unrelated session descriptions'}
    if([IO.File]::ReadAllText($sentinel) -ne $sentinelText){throw 'Descriptor removal changed the launch working-directory sentinel'}
    Assert-Identity (Invoke-Native 'status' (Session-Arguments $one)) $one $activeHostPID
    Assert-Identity (Invoke-Native 'status' (Session-Arguments $two)) $two $activeHostPID
    $result.checks+='rename and stopped-description removal preserve project sentinel and both live session identities'

    $shutdown=Invoke-Native 'shutdown'
    if($shutdown.running -or $shutdown.host_pid -ne $manager.host_pid){throw 'Shutdown acknowledgement has incorrect manager identity/liveness'}
    Wait-ProcessesGone @($one.pid,$two.pid,$manager.host_pid)
    $activeHostPID=0
    Remove-OwnedTask $manager.task_name
    $result.checks+='manager shutdown acknowledges native cleanup; both owned roots and host are gone'

    $reopened=Start-IndependentManager 'reopen'
    if($reopened.host_pid -eq $manager.host_pid){throw 'Reopened manager did not get a new process identity'}
    $recovered=@($reopened.sessions)
    if($recovered.Count -ne 2){throw 'Reopened registry lost persisted descriptions'}
    foreach($expected in @($one,$two)){
        $match=@($recovered|Where-Object {$_.id -eq $expected.id})
        if($match.Count -ne 1 -or $match[0].name -ne $expected.name -or $match[0].dir -ne $expected.dir -or $match[0].state -ne 'stopped' -or [int]$match[0].pid -ne 0 -or $match[0].run_id -or $match[0].host_pid -ne $reopened.host_pid){throw 'Recovered descriptor was altered or automatically launched'}
    }
    if(@(Get-CimInstance Win32_Process -Filter ('ParentProcessId='+$reopened.host_pid)).Count -ne 0){throw 'Reopened manager unexpectedly has native child processes'}
    if([IO.File]::ReadAllText($sentinel) -ne $sentinelText){throw 'Manager recovery changed project contents'}
    $result.reopened_host_pid=$reopened.host_pid
    $result.recovered_sessions=$recovered
    $result.checks+='same private registry reopens with two renamed/persisted stopped descriptions, PID zero and no automatic launch'
    $finalShutdown=Invoke-Native 'shutdown'
    if($finalShutdown.running -or $finalShutdown.host_pid -ne $reopened.host_pid){throw 'Reopened manager shutdown was not acknowledged exactly'}
    Wait-ProcessesGone @($reopened.host_pid)
    $activeHostPID=0
    Remove-OwnedTask $reopened.task_name
    $result.sessions=@($one,$two)
    $result.passed=$true
} catch {
    $result.error=$_.Exception.Message
} finally {
    if($cleanupRequested){
        if($activeHostPID -gt 0){try {Invoke-Native 'shutdown'|Out-Null} catch {}}
        foreach($taskName in @($taskNames)){
            try {Remove-OwnedTask $taskName} catch {$result.passed=$false;$result.cleanup_error=$_.Exception.Message}
        }
        try {Wait-ProcessesGone @($ownedProcessIds)} catch {$result.passed=$false;$result.cleanup_error=$_.Exception.Message}
        $remainingTasks=@($taskNames|Where-Object {Get-ScheduledTask -TaskName $_ -TaskPath '\' -ErrorAction SilentlyContinue})
        $result.task_removed=($remainingTasks.Count -eq 0)
        if(-not $result.task_removed){$result.passed=$false;$result.cleanup_error='An exact temporary task remains'}
    }
    $result.task_names=@($taskNames)
    $result.completed_utc=[DateTime]::UtcNow.ToString('o')
    $json=$result|ConvertTo-Json -Depth 8 -Compress
    if($ReportPath){[IO.File]::WriteAllText($ReportPath,$json,(New-Object Text.UTF8Encoding($false)))}
    Write-Output $json
}
if(-not $result.passed){exit 1}
