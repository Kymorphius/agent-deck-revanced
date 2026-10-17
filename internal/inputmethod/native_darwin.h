// matrix/hardened: private native bridge for local terminal input sources.
int deck_terminal_owner(void);
int deck_parent_pid(int pid);
int deck_is_foreground(int owner);
char *deck_source_id(int ascii);
int deck_select_source(const char *identifier, int owner);
