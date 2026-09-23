---
memo: 1
alias: vybava-team
kind: team
---
<!-- - #<id> <type>/<topic>[!] <sentence> [-> <link> ...] ^m<id>  (team ledger: #t<id> ... ^t<id>) -->
- #t1 project/dev-binaries Other sessions' dev builds overwrite ~/.local/bin/vybava within minutes; after a guards change rebuild from main into every ~/.local/share/vybava/bin/vybava-*-dev too, then run claude-guards doctor. ^t1
- #t2 project/claude-guards A guard that must never be bypassed reads the command as text and fails closed; shell parsing lost commits behind if/then/eval and subshell cd that a substring trigger caught. -> https://github.com/henderson-tech/vybava/pull/83 ^t2
