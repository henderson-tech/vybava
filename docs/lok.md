# lok

lok owns locale catalogs the way an AI session should touch them: never
whole. A 5 000-key `cs.json` is ~100k tokens of strings a session will not
use; reading it to add one key, or rewriting it through the shell, is the
single largest avoidable context sink in a localized app. lok makes the
catalog a store — queried by key, written by verb, every locale kept in
sync by construction — and `claude-guards` refuses raw reads of any file
the config declares as a catalog.

Configuration lives in the `lok` section of `vybava.config.ts`
([docs/config.md](config.md)). Two key styles:

```text
english-as-key   flat JSON, the key IS the English source text (t('Save'));
                 an `en` file, when present, maps key → key and is derived
path             nested JSON addressed by dotted path (meta.title)
```

Each catalog names its `files` pattern (`{locale}`), its `locales`, the
`required` subset every key must carry (others are tracked, never silently
missing), optional `plurals` suffixes, `exempt` regexes for structured keys whose en value is prose (`_help$`), an `afterWrite` command (a type
generator, a formatter) and, for english-as-key catalogs, a `scan` block
and a `mirrors` block (see [Renaming keys](#renaming-keys-lok-mv-and-sub---keys)).

## Key grammar

An english-as-key key is the English text itself, verbatim: `Save.` is a
key, and nothing in it is ever escaped or parsed.

A path key is its segments joined by `.`. A JSON key that itself holds a
dot (an API failure code such as `bankid.user_not_eligible`) is ONE segment,
written with the dot escaped as `\.`; a literal backslash is `\\`. Any other
backslash sequence is refused (`CONFIG_INVALID` "bad key escape"), which
keeps room for future escapes:

```text
account.onboarding.identity.status.failed.codes.bankid\.user_not_eligible.description
```

Every key lok prints (grep hits, get and write receipts, `missing` gaps,
`check` problems, merge clashes, `next` and fix lines) is in this canonical
form, and every verb reads it back, so a printed key always pastes into
`get`/`set`/`rm`. Single-quote it: an unquoted shell drops the backslash.
When a key is missing, lok searches the tree loosely (every dot a possible
separator) and, if exactly one leaf matches, the `KEY_MISSING` fix is that
leaf's exact escaped command; it is a diagnostic only and never resolves a
key silently. A write that would create a new object `bankid` beside dotted
siblings `bankid.*` is refused and names the escaped key it meant.

```text
lok catalogs --json                       # every catalog, key counts, gaps per locale
lok get 'Save' --json                     # one key across locales (plural variants included)
lok grep 'inquir' --limit 20 --json       # regex over keys+values, always capped
lok missing --json                        # required-locale gaps; --all for every locale
lok check --json                          # parity + english-as-key + {{placeholder}} invariants — CI gate

lok add 'Cancel' --tr cs='Zrušit' --json  # inserts at the alphabetical slot in EVERY locale
lok set 'Cancel' --tr cs='Storno' --json  # updates the given locales only
lok rm 'Cancel' --json                    # removes the key + its plural variants everywhere
lok rm '{{n}} weeks_few' --locale en      # only in the given locales (an inert en variant, cs plurals kept)
lok scan --json                           # literal t('…') keys missing from the catalog + probable orphans
lok scan --write --json                   # add the missing (en = key) → translate via `lok missing`

lok sub '\.\.\.' '…' --json               # regex rewrite over values: a dry run, next = the exact write
lok sub '\.\.\.' '…' --write --expect 380 # apply exactly the reviewed count
lok mv 'Loading...' 'Loading…' --json     # rename an english-as-key family + its t() call sites
```

In an english-as-key catalog `--tr en=…` is refused for a base key (the en
value IS the key) but accepted for a plural variant or an `exempt` key, so
`{{count}} item_one` can read "1 item" while `_other` reads "{{count}}
items". A variant written without it derives the literal key and is flagged
by `lok missing` (a gap with `warning`) and `lok check` (`en-unworded`,
severity `warning` — never fails the gate) until it is worded.

Writes preserve the file's key order and insert new keys at the
case-insensitive alphabetical slot without reordering anything else, so a
diff shows exactly the change. Only files whose content changed are
rewritten; `afterWrite` runs once per write from the repo root.

`scan` is deliberately asymmetric. A literal `t('…')` absent from the
catalog is always a defect, so `--write` adds it. A catalog key never seen
verbatim in source is only a hint — keys held in lookup tables, API
messages passed through `t()`, template strings — so orphans are listed
with a count and never deleted; `lok rm` is the explicit path.

Test sources are never scanned — `*_test.go`, a `.test.`/`.spec.` segment
anywhere in the name (`a.test.ts`, `a.spec.gen.ts`), `__tests__/` and
`testdata/`: a test asserts copy, it never defines a key, so its synthetic
`T("Hello {{name}}")` is neither added nor counted as usage. Neither are
`.d.ts` declaration files: they hold types, never a call, and a generated
key union (`translation-keys.d.ts`) quotes every key and would hide every
orphan. Calls inside
`//` and `/* */` comments are not extracted either (a doc comment's example
call is not a key), including comments inside a template's `${…}`; a `//`
or `/*` inside a string, template text, a regex literal or a URL stays code.
Go is lexed exactly: a `/*` there is always a comment. The JS/TS lexer is
not a parser, and without one regex-vs-division is genuinely ambiguous. It
decides by the token before the `/`: an operator, an expression keyword or
the `)` of an `if`/`while`/`for`/`with` condition opens a regex, and any
other token divides. JSX text is lexed as code, and a JSX `{/* … */}` is a
comment. A known tail remains, almost always confined to one line:

```text
// in JSX text not after ':'  <p>a // b {t('x')}</p>   rest of line blanked, key dropped
/* in JSX text after a word    <p>src/* {t('x')}</p>    read as code when left open on its line
/* in JSX text after a tag/}   <p>{a} /* b</p>          comment up to the next */, keys dropped
a regex after a missed token   default /[//]/           as the // or /* rows above
```

`scan.call` names the call shapes, one string or a list (default `t`). A
bare name matches `t('…')` with nothing dotted before it — `foo.t(` is not
a hit. The method form `*.T` matches `.T('…')` on any receiver, which is
how Go and class-based code translate: `l.T("Sites")`,
`i18n.FromContext(ctx).N("{{count}} items", n)`. One list scans both
languages into one catalog; add the extension the defaults lack:

```ts
scan: { roots: ['apps/client', 'services/api'], call: ['t', '*.T', '*.N'], extensions: ['.ts', '.tsx', '.go'] }
```

`--catalog=<id>` is required only when the destination cannot be inferred.
Reads and `set`/`rm` resolve the one catalog holding the key. `add` resolves
a NEW key in this order: the one catalog already holding it (→ `KEY_EXISTS`),
then the path catalog whose existing parent path is longest
(`account.gigWorker.form.newBadge` lands where `account.gigWorker.form`
lives), then — when the key contains a space — the single english-as-key
catalog. A tie or a miss is `CATALOG_AMBIGUOUS` naming only the tied
candidates; use the `--catalog=<id>` form, which survives every shell's
word splitting.

Every write returns a receipt — `{catalog, key, locales, written,
afterWrite: {cmd, ok}}` — so a generated type (`afterWrite`) is never a
silent side effect; a failing `afterWrite` still returns the receipt next to
`AFTER_WRITE_FAILED`.

Every write lands through `<file>.lok-tmp` + rename, all locales of a
catalog or none, and refuses `CATALOG_CHANGED` when a file no longer holds
what lok loaded (another session wrote it meanwhile); rerun the command.

## Rewriting values: `lok sub`

```text
lok sub <pattern> <replacement> [--catalog=a,b] [--locale cs,sk] [--key <re>] [--exclude-key <re>]
        [-F|--literal] [-i|--ignore-case] [--limit 20] [--refs] [--json]    # dry run (default)
lok sub <pattern> <replacement> ... --write --expect <n>                     # apply
```

`sub` runs Go RE2 `ReplaceAllString` over every string value in scope (array
items included; the pattern never sees the key). It is a dry run by default:
the output is the review, one `-`/`+` block per change with 30 runes of
context, and its `next` is the exact `--write --expect <n>` command. The
write refuses `SUB_DRIFT` unless exactly n values change, so the count
written is the count reviewed (`--write` without `--expect` refuses
`EXPECT_REQUIRED`). `--limit` caps the listed changes, and in a key rename
the listed renames, call sites and leftovers (totals
stay complete; 0 lists none); `--refs` lists test sources (the scan roots,
or the catalog's app directory, plus `e2e/` and `appium/`) that hold an old
value verbatim, the tests a copy change breaks.

In an english-as-key catalog the `en` value of a base key IS the key, so it
is skipped and reported (`skipped[]`, reason `en-is-key`); renaming the key
is `--keys` (below). En plural variants and exempt keys carry real wording
and are rewritten.

Matching is case-sensitive by default, unlike `grep`: a rewrite writes the
replacement literally, so `(?i)` would lowercase every sentence-initial
match. Spell casing variants out (`([eE])-mail` → `${1}-mail`) or pass `-i`.

RE2 and template notes:

```text
$1 ${1} ${name} $$   templates; write ${1}a, never $1a (Go reads the group
                     named "1a" and expands it to nothing: BAD_REPLACEMENT)
'single quotes'      around the replacement, or the shell eats $1 (a pattern
                     with groups and a replacement without `$` warns)
\x{00A0} \\          decoded in the replacement: type an NBSP, a non-breaking
                     hyphen (\x{2011}) or a long dash visibly
\b \w                ASCII-only in RE2: \bmáš\b never matches Czech text;
                     use (^|\PL)máš(\PL|$) and re-emit ${1}
no lookaround        and no backreferences; --exclude-key is the carve-out
-F                   the pattern is literal text (\x{...} still decoded), the
                     replacement takes no templates
```

Safety model: every check runs in memory over every catalog in scope
before a byte is written, and any refusal writes nothing anywhere.

```text
PLACEHOLDER_CHANGED  a changed value's {{x}} / {x} multiset differs
VALUE_EMPTIED        a non-empty value would become empty or whitespace
CHECK_REGRESSED      the rewritten catalog fails a `lok check` rule it passed
CATALOG_CHANGED      a locale file of a catalog in scope changed on disk since load
EXPECT_REQUIRED      sub --write without --expect (mv takes no count)
SUB_DRIFT            --expect differs from the count that would change now
BAD_PATTERN          not RE2 (or a bad --key / --exclude-key)
BAD_REPLACEMENT      a template names no group, or a bad \x{...}
```

Each catalog lands atomically and its `afterWrite` runs once; across
catalogs a write is best effort and the receipt (`byCatalog[].written`)
names what landed. `stillMatching` counts rewritten values the pattern
matches again (one-letter words in a row share the separator a match
consumed); `next` then offers a second pass. Human output shows NBSP,
U+2011, U+202F, U+200B, the long dashes and other invisible runes as
`\x{...}`; `--json` keeps raw strings.

Recipes (a project's i18n skill keeps its own table of them):

```text
lok sub ' ?[\x{2013}\x{2014}] ?' ' - '                 long dashes in values
lok sub '(\d)\x{2013}(\d)' '${1}-${2}'                  a number range
lok sub '\.\.\.' '…'                                    ellipsis in values (en keys are skipped)
lok sub '\.\.\.' '…' --keys --catalog=mobile --merge    ellipsis in english-as-key keys + call sites
lok sub '„([^"“”„]*)"' '„${1}“' --locale cs,sk          a Czech closing quote
lok sub '(\d) (Kč|%)' '${1}\x{00A0}${2}' --locale cs    NBSP between a number and its unit
```

## Renaming keys: `lok mv` and `sub --keys`

```text
lok mv <old-key> <new-key> [--catalog=<id>] [--merge] [--with-mirrors] [--no-source] [--allow-literals] [--write]
lok sub <pattern> <replacement> --keys [--merge] [--with-mirrors] [--no-source] ... [--write --expect <n>]
```

For english-as-key catalogs only (a path key is reached through generated
types: `KEYS_UNSUPPORTED`, use `lok set` + `lok rm` + the typecheck). `mv`
moves one family; `sub --keys` applies the regex to every base key in scope
and `--expect` counts families. Without `--catalog`, `mv` moves the family in
every english-as-key catalog holding it. Both keys of `mv` decode `\x{HHHH}`.

- **Family move.** The base key and each plural variant move per locale,
  exactly the variants that locale holds (cs keeps `_few`/`_many`, en keeps
  `_one`/`_other`; nothing is manufactured). Keys land at the sorted slot,
  like `add`.
- **Values.** Translations stay. An en value equal to its key follows the
  key; a worded en variant takes the same regex under `sub --keys` and is
  left alone by `mv`. The old and new key must carry the same placeholder
  set (`PLACEHOLDER_CHANGED`): call sites pass values by name.
- **Collisions.** A new key that already exists is `KEY_EXISTS`, listing the
  locales that differ; when every locale's family is identical, `--merge`
  drops the old family and repoints its call sites. Two keys renamed onto
  one is `KEY_EXISTS` too.
- **Call sites** (catalogs with `scan`). Every literal `t('old')` under the
  scan roots is rewritten in place, re-escaped for its quote style; test
  files are rewritten and reported as `test`. Any other quoted `'old'` /
  `"old"` left there (a lookup table, `t(cond ? 'a' : 'b')`, an `i18nKey`
  prop, a test assertion) is listed with file:line, and `--write` refuses
  `CALL_SITES_UNRESOLVED` until it is resolved or `--allow-literals`. A
  catalog with neither `scan` nor `mirrors` refuses `NO_SCAN`; `--no-source`
  skips the call sites when a typecheck names every stale one.
- **Mirrors.** `mirrors: { roots: ['apps/api/src'] }` marks keys that copy a
  source literal (API error sentences a client translates). A rename finds
  the exact quoted literal in the non-spec files under those roots and
  refuses `MIRROR_SOURCE` (file:line) unless `--with-mirrors`, which rewrites
  it and moves the key in every mirror catalog holding it in the same run.
  Spec files asserting the old text are listed, never rewritten.
  `bundledInStoreApp: true` marks a catalog that ships inside a store app
  binary while the source deploys on its own: every rename warns that
  store-live apps keep the old key (retire it in three steps: list both
  texts, switch the source, drop the old key a release later). lok cannot
  see the store version and never decides this for you.

After a write, `next` is `lok check --catalog=<id> --json` (plus `lok scan`
for a scanned catalog, which must no longer list the old key as missing).

## Merging catalogs

`lok merge-driver %O %A %B %P` is the git merge driver for declared catalogs
(`merge-assist setup` registers it; see [merge-assist.md](merge-assist.md)).
Git calls it when both sides of a merge touched the same catalog file. It
merges by key, not by line:

```text
equal on both sides            kept
changed or deleted on one side that side wins (a deletion is honoured, never resurrected)
changed differently on both    CLASH: the file stays conflicted, the keys are printed
two objects (path style)       merged key by key, recursively
```

The result keeps theirs' order (the branch merged in, usually main) and
inserts ours' additions where `lok add` would put them on theirs, so the
merged file diffs against main by exactly the branch's changes. The file is
only written when theirs round-trips byte-for-byte through lok's writer;
anything else (another indent, CRLF), an undeclared path and a missing
config all fall back to git's own text merge, so the driver never loses
behaviour. When the merged catalog differs from theirs, its `afterWrite` is
queued for `merge-assist regen`.

A clash is always a conflict, even when git's line merge would be clean:
two branches adding the same key worded differently at different lines
merge "cleanly" into a file holding the key twice, where the last copy
silently wins. Settle it without reading the file:

```text
lok merge apps/web/i18n/strings.cs.json --prefer theirs --json   # re-merge from the index stages, write, git add
lok set 'Save' --tr cs='Uložit'                                   # then word the keys that need a mix
```

`lok merge <path>` without `--prefer` re-merges an unmerged catalog left by
a merge that ran without the driver; it fails `MERGE_CLASH` listing the
clashing keys with their base, ours and theirs values.
