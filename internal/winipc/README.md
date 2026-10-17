# Private Windows IPC

`Listen` and `Dial` use Microsoft/go-winio byte-stream named pipes. Names must
start with `\\.\pipe\agent-deck-native-` and have a 1–128 character suffix made
of ASCII letters, digits, underscores, and hyphens. No TCP listener or remote
UNC path is accepted. Non-Windows hosts return `ErrUnsupported`.

The listener's protected DACL grants full access only to the current process
user SID and SYSTEM. go-winio uses `FILE_PIPE_REJECT_REMOTE_CLIENTS` on every
server pipe handle and refuses an already-existing pipe name. Before returning
a connection, `Dial` checks the server process token has the caller's user SID;
another local account cannot impersonate the expected server by reserving its
name. This API does not distinguish programs running as the same account.
Local administrators and SYSTEM retain their OS privileges; this is not a
security boundary against administrators.

`ProtectPath` changes the DACL of one explicit, existing state file or directory.
It requires an ordinary local drive-absolute path, rejects final-component
reparse points and multiply-linked files, and checks ownership against the
current user or the process token's default owner. The latter allows an elevated
administrator's dedicated files to remain owned by Administrators. The owner
is preserved; the resulting protected DACL grants access only to the user SID
and SYSTEM, then is independently read back.

The path's ancestors must already be trusted. Callers must pass their dedicated
state directory or explicitly owned state file, rather than a profile or shared
directory, and protect a new directory before creating sensitive contents.
A handle opened with `MAXIMUM_ALLOWED` prevents inheritable directory ACEs from
being propagated to preexisting children, as documented by
[Microsoft's SetSecurityInfo documentation](https://learn.microsoft.com/en-us/windows/win32/api/aclapi/nf-aclapi-setsecurityinfo).
Exclusive sharing alone did not prevent propagation in native Windows Server
2025 testing; the implementation also retains exclusive sharing to pin the
target while verifying and changing its ACL.
Existing child files retain their ACLs and need their own explicit calls; new
children inherit the restricted parent ACL. The function does not traverse,
create, or remove files. A busy object can fail with a sharing violation.

Portable tests run through `scripts/run-isolated.py go test ./internal/winipc`.
Windows tests require the explicit environment variable `AGENTDECK_NATIVE_TEST=1`
and touch only unique named pipes and temporary state directories. They check
Unicode round trips, duplicate-name rejection, cancellation, native ACL
readback, data preservation, existing-child ACL scope, future-child inheritance,
and hard-link/reparse-point rejection. The symlink subcheck reports when the
test account lacks permission to create symlinks. Remote-client rejection is
verified against the pinned library implementation; these tests do not make
SMB/network connections or create another Windows account.
