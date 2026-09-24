#!/bin/sh
# Release-readiness lanes (skill release-readiness, from the Reservine run, lane vt-1008): give
# every Playwright attachment a unique file name and rewrite the JUnit to point at the copies.
# Playwright names them all `test-finished-1.png`, and vitrinka maps uploaded shots by basename
# (fixit/vitrinka#3122), so board cards show the wrong test's shot. Idempotent: it always starts
# from the pristine JUnit. Retire it once #3122 keys shots by full path.
# Usage: sh uniq-shots.sh <.vitrinka/runs dir> [junit file name, default playwright-junit.xml]
set -eu
R="${1:?usage: uniq-shots.sh <.vitrinka/runs dir> [junit file name]}"
J="$R/${2:-playwright-junit.xml}"
[ -f "$J" ] || { echo "no JUnit at $J" >&2; exit 1; }
[ -f "$J.orig" ] || cp "$J" "$J.orig"
rm -rf "$R/shots" && mkdir -p "$R/shots"

# One injective mapping for the copy and the rewrite: every _ becomes _1, then every
# / becomes _2 (a plain / → __ would map a/b__c and a__b/c to the same name).
grep -o '\[\[ATTACHMENT|[^]]*\]\]' "$J.orig" | sed 's/^\[\[ATTACHMENT|//; s/\]\]$//' | sort -u |
  while IFS= read -r p; do
    [ -f "$R/$p" ] || { echo "missing: $p" >&2; continue; }
    cp "$R/$p" "$R/shots/$(printf '%s' "$p" | sed 's/_/_1/g; s#/#_2#g')"
  done

perl -pe 's#\[\[ATTACHMENT\|([^\]]+)\]\]#"[[ATTACHMENT|shots/" . (($1 =~ s{_}{_1}gr) =~ s{/}{_2}gr) . "]]"#ge' "$J.orig" > "$J"

missing=0
for f in $(grep -o 'ATTACHMENT|shots/[^]]*' "$J" | sed 's/^ATTACHMENT|//' | sort -u); do
  [ -f "$R/$f" ] || { echo "unresolved: $f" >&2; missing=$((missing + 1)); }
done
[ "$missing" -eq 0 ] || { echo "$missing rewritten attachment(s) have no copied file" >&2; exit 1; }

echo "shots: $(ls "$R/shots" | wc -l | tr -d ' ') unique files; attachments in junit: $(grep -o 'ATTACHMENT|shots/' "$J" | wc -l | tr -d ' ')"
