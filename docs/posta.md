# posta

Drive a shared test mailbox so an agent can finish a real email journey on its
own: sign-up verification, password reset, an attachment round trip. The
application under test keeps sending through its normal provider — posta is only
ever the recipient, and the human never has to read a code out loud.

```sh
posta address --project fixit --role customer      # mint this run's address
posta wait    --to <addr> --since 10m --json       # block until the mail lands
posta links   --to <addr> --match '/reset'         # just the call to action
posta attach  --to <addr> --save ./downloads       # files to disk
posta send    --to someone@example.com --subject S --body B
posta purge   --to <addr>                          # empty a run's address
posta doctor                                       # prove IMAP + SMTP authenticate
```

## Credentials

The mailbox address and its app password arrive in `POSTA_ADDRESS` and
`POSTA_APP_PASSWORD`. Inject them from the vault at the point of use — never a
literal, never a file, never a shell export:

```
onyx run_command
  argv     ["posta", "wait", "--to", "<addr>", "--json", "--out", "/tmp/mail.json"]
  env_refs POSTA_ADDRESS      onyx://<group>/<item>/username
           POSTA_APP_PASSWORD onyx://<group>/<item>/app_password
```

Bind the vault item with `allowed_commands` pointing at the installed `posta`
path, so the credential cannot be injected into anything else.

A missing password is a hard error rather than a prompt or a fallback: posta is
meant to run unattended, and a fallback would be a way to run it against the
wrong mailbox.

**`--out` is not optional in that context.** `onyx run_command` redacts a
child's entire stdout the moment it injects a credential, so a caller that does
not pass `--out` gets an empty string back and no message. Write the result to a
file and read the file; a failed command leaves `{"error": …}` there instead.

Gmail needs an app password from <https://myaccount.google.com/apppasswords>,
which in turn needs 2-Step Verification enabled on the account. Passkeys can stay
on — they gate the browser login, not IMAP or SMTP. `POSTA_IMAP_ADDR` and
`POSTA_SMTP_ADDR` point the applet at a non-Gmail mailbox.

## Addressing

Gmail delivers every `local+tag@` form to `local@`, so one mailbox provides
unlimited per-test inboxes with nothing to provision. `posta address` mints
`<local>+<project>-<role>-<run>@<domain>`, with a random run id when none is
given, and flattens a base that already carries a tag rather than nesting a
second one.

A freshly minted address has never received mail. That is the real guarantee: a
run cannot be satisfied by a previous run's message, and two agents driving the
same journey in parallel never read each other's reset links.

The application under test must keep the **whole** address when it identifies
the user. Stripping the `+tag` collapses every run back into one account.

## Matching

`wait` and `list` narrow on the server by date only, because `SINCE` is the one
criterion every provider implements identically; recipient and subject are then
matched here against the real headers. A server-side header search that silently
under-matches is indistinguishable from "the mail never arrived", which is the
one failure this tool must not have.

- The recipient comparison is the whole tagged address, `Delivered-To` first and
  `To`/`Cc` as the fallback, case-insensitive.
- `--since` is compared against the delivery time, not the sender's `Date:`
  header, so a sender with a wrong clock cannot hide its own mail. Take the
  timestamp before the action that triggers the mail and pass it in.
- `wait` returns the **newest** match: when a journey requests two reset mails,
  only the latest token is still valid.
- `links` puts anchor hrefs before bare URLs and drops duplicates, so `links[0]`
  is reliably the call to action rather than the unsubscribe footer.

## Mailboxes

The default folder is `[Gmail]/All Mail`, not `INBOX`. An inbox is a label and a
label can be missed — Gmail keeps self-addressed mail out of the inbox entirely,
files bulk senders under Promotions, and honours any filter on the account — so
a journey waiting on `INBOX` reports "never arrived" for mail that arrived
perfectly well. All Mail holds every delivered message.

The trade-off is that All Mail also holds the mailbox's own sent copies, so a
test that must prove *inbox* delivery specifically should pass `--mailbox INBOX`
and accept the label risk knowingly.

## Purging

`purge` refuses any recipient without a `+tag`. The mailbox is a real account
with real mail in it, and a tagged address is one posta minted for a test run, so
purge is structurally unable to delete anything a human cares about no matter
what the caller passes.
