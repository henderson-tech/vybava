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
generator, a formatter) and, for english-as-key catalogs, a `scan` block.

```text
lok catalogs --json                       # every catalog, key counts, gaps per locale
lok get 'Save' --json                     # one key across locales (plural variants included)
lok grep 'inquir' --limit 20 --json       # regex over keys+values, always capped
lok missing --json                        # required-locale gaps; --all for every locale
lok check --json                          # parity + english-as-key + {{placeholder}} invariants — CI gate

lok add 'Cancel' --tr cs='Zrušit' --json  # inserts at the alphabetical slot in EVERY locale
lok set 'Cancel' --tr cs='Storno' --json  # updates the given locales only
lok rm 'Cancel' --json                    # removes the key + its plural variants everywhere
lok scan --json                           # literal t('…') keys missing from the catalog + probable orphans
lok scan --write --json                   # add the missing (en = key) → translate via `lok missing`
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
`T("Hello {{name}}")` is neither added nor counted as usage. Calls inside
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
