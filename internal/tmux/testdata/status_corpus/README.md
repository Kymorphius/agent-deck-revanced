# Pane status fixtures

These 117 pane frames preserve captured or source-derived terminal layouts for
status detection across Claude, Codex, pi, and shell sessions. The corresponding
`labels.tsv` entry records each frame's harness, expected verdict, and relevant
UI behavior. Every frame and verdict from the original corpus is retained.

Before publication, filenames were replaced by neutral per-harness frame numbers.
All payload prose, session titles, personal subjects, account labels, paths,
addresses, identifiers, dates, and nonterminal emoji were masked. Repeated `x`,
`X`, `0`, and neutral wide characters are synthetic payload placeholders, not
captured user text. No mapping to original session names is included.

Terminal control and status vocabulary remains in its original location,
including occurrences quoted in completed prose or tool output. Whitespace,
line breaks, indentation, box rules, composer placement, queued-input rows,
wrapped status details, roster lengths, permission menus, error cues, and live
status fields remain to exercise the original regression conditions. Account
usage and cost fields use synthetic numeric values with the same shape.
