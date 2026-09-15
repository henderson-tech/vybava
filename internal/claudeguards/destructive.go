package claudeguards

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// guardDestructive — the three incident-born hard bans:
//   1. git stash            — banned everywhere, except read-only list/show
//   2. git checkout/switch/restore-dot — banned in the user's primary clone
//   3. volume-destroying docker (compose down -v, volume rm on DB, prunes)
//
// Escape hatch: prefix the command with CLAUDE_ALLOW_DANGEROUS=1 (ask the
// user first).
// ---------------------------------------------------------------------------

const destructiveEscape = "If this is genuinely intended, ASK THE USER FIRST, then re-run the command prefixed with CLAUDE_ALLOW_DANGEROUS=1"

// `git` plus any global flags (-C <path>, -c k=v, --flag[=v]) before the subcommand.
const gitRe = `(^|[[:space:]])git([[:space:]]+(-C[[:space:]]+[^[:space:]]+|-c[[:space:]]+[^[:space:]]+|--[a-z-]+(=[^[:space:]]+)?))*[[:space:]]+`

var (
	reStash        = regexp.MustCompile(gitRe + `stash([[:space:]]|$)`)
	reStashRO      = regexp.MustCompile(gitRe + `stash[[:space:]]+(list|show)([[:space:]]|$)`)
	reSwitch       = regexp.MustCompile(gitRe + `(checkout|switch)([[:space:]]|$)`)
	reRestoreDot   = regexp.MustCompile(gitRe + `(checkout|restore)([[:space:]]+--[a-z-]+)*[[:space:]]+\.([[:space:]]|$)`)
	reComposeDown  = regexp.MustCompile(`(^|[[:space:]])docker([[:space:]]+compose|-compose)([[:space:]]+[^[:space:]]+)*[[:space:]]+down([[:space:]]|$)`)
	reVolumesFlag  = regexp.MustCompile(`([[:space:]]-[a-zA-Z]*v([[:space:]]|$)|[[:space:]]--volumes([[:space:]]|$))`)
	reVolumeRm     = regexp.MustCompile(`(^|[[:space:]])docker[[:space:]]+volume[[:space:]]+rm([[:space:]]|$)`)
	reDBVolumeName = regexp.MustCompile(`(?i)(postgres|pgdata|timescale|mysql|mariadb|mongo|db[_-]data|[_-]db([[:space:]]|$))`)
	reVolumePrune  = regexp.MustCompile(`(^|[[:space:]])docker[[:space:]]+volume[[:space:]]+prune([[:space:]]|$)`)
	reSystemPrune  = regexp.MustCompile(`(^|[[:space:]])docker[[:space:]]+system[[:space:]]+prune([[:space:]]|$)`)

	// macOS `security` invocations that print a stored SECRET VALUE:
	// find-*-password -w/-g echo the password; dump-keychain -d decrypts
	// every item. Metadata-only lookups (no value flag) stay allowed.
	reSecurityFind  = regexp.MustCompile(`(^|[[:space:]])security[[:space:]]([^;|&]*[[:space:]])?find-(generic|internet)-password([[:space:]]|$)`)
	reSecretValFlag = regexp.MustCompile(`[[:space:]]-(w|g)([[:space:]]|$)`)
	reDumpKeychain  = regexp.MustCompile(`(^|[[:space:]])security[[:space:]]([^;|&]*[[:space:]])?dump-keychain([[:space:]]|$)`)
	reDumpDecrypt   = regexp.MustCompile(`[[:space:]]-d([[:space:]]|$)`)
)

// A worktree stack is a per-branch throwaway database, so dropping its volumes
// is routine — but only when the command is provably aimed at one. Both
// worktree layouts are in daily use (`wt-<slug>` and `wk-<slug>`), and the
// trustworthy evidence is the project the call NAMES (-p / --project-name) or a
// worktree directory carrying the prefix. Matching "wt-" anywhere in the string
// (the pre-2026-09-15 rule) let an unrelated argument — a filename, a comment —
// disarm a data-loss guard.
var (
	reWorktreeProject = regexp.MustCompile(`(^|[[:space:]])(-p[[:space:]]*|--project-name[[:space:]]*=?[[:space:]]*)["']?w[tk]-`)
	reWorktreePath    = regexp.MustCompile(`(^|/)w[tk]-`)
)

func worktreeStack(seg, cwd string) bool {
	return reWorktreeProject.MatchString(seg) || reWorktreePath.MatchString(cwd)
}

// Directories where branch switching is fine: worktrees (both the custom
// /wk:* layout and Claude Code's native isolation layout) and throwaway clones.
func exemptDir(cwd string) bool {
	if strings.Contains(cwd, "/.worktrees/") || strings.Contains(cwd, "/.claude/worktrees/") {
		return true
	}
	for _, p := range []string{"/tmp/", "/private/tmp/", "/var/folders/"} {
		if strings.HasPrefix(cwd, p) {
			return true
		}
	}
	return false
}

// destructiveRule is one table entry: fire() decides on a single segment
// (with cwd for directory carve-outs); msg is the reason Claude sees.
type destructiveRule struct {
	name string
	fire func(seg, cwd string) bool
	msg  string
}

var destructiveRules = []destructiveRule{
	{
		name: "git-stash",
		fire: func(s, _ string) bool {
			return reStash.MatchString(s) && !reStashRO.MatchString(s)
		},
		msg: `git stash is banned in all mutating forms (list/show are allowed) — not
path-scoped, not briefly held, not even on your own edits "just to compare against HEAD".

The user runs multiple sessions against one clone. A stash mutates the SHARED
working tree, so every parallel session watches files revert under its feet and
misreads it as intentional user edits.

Do this instead:
  • commit the change (straight to main is the documented preference for deploy work)
  • build from a clean worktree
  • materialize HEAD outside the tree:
      git archive HEAD <paths> | tar -x -C <scratchpad>
      git show HEAD:<file>
  • or just accept the tool default (EAS Build with cli.requireCommit=false
    uploads uncommitted changes anyway — no stash needed)`,
	},
	{
		name: "git-switch",
		fire: func(s, cwd string) bool {
			return !exemptDir(cwd) && reSwitch.MatchString(s)
		},
		msg: `git checkout / git switch is banned in the user's primary checkout.

Their checkout is their seat: it may hold uncommitted work, running dev servers
bound to this branch, and IDE state. Switching under them silently mutates it.

Use a worktree instead (creation is user-gated — ask first):
  git worktree add .worktrees/<name> -b <branch>
  cd .worktrees/<name>

Inspecting another ref needs no checkout: git show <ref>:<file>, git log -p.
This guard does not fire inside .worktrees/, .claude/worktrees/, /tmp, or /private/tmp.`,
	},
	{
		name: "git-restore-dot",
		fire: func(s, cwd string) bool {
			return !exemptDir(cwd) && reRestoreDot.MatchString(s)
		},
		msg: `git checkout . / git restore . destroys uncommitted work in the
user's shared working tree — including other sessions' in-flight edits.

Revert only what YOU changed, by explicit path, or leave it alone.`,
	},
	{
		name: "compose-down-volumes",
		fire: func(s, cwd string) bool {
			if !reComposeDown.MatchString(s) || !reVolumesFlag.MatchString(s) {
				return false
			}
			// Carve-out: a wt-/wk- worktree stack, named by the command or by
			// the worktree directory the call runs in.
			return !worktreeStack(s, cwd)
		},
		msg: `docker compose down -v DESTROYS VOLUMES — all database data, permanently.

Safe alternatives:
  docker compose down        # keeps volumes
  docker compose restart postgres

Back up first if this touches a DB: pnpm db:backup (lovinka).

The only carve-out is a worktree stack — name it and the guard stands aside:
  docker compose -p wt-<slug> down -v      # or -p wk-<slug>
(or run from a wt-/wk- prefixed worktree directory). A bare down -v inside a
worktree is NOT carved out: it resolves COMPOSE_PROJECT_NAME from that tree's
.env, which is how a copied .env dropped another stack's volumes.`,
	},
	{
		name: "volume-rm-db",
		fire: func(s, _ string) bool {
			return reVolumeRm.MatchString(s) && reDBVolumeName.MatchString(s)
		},
		msg: `docker volume rm on a database volume permanently deletes all data.

There is no undo. If the goal is a clean restart:
  docker compose restart postgres

If postgres init is failing, fix the SQL/permissions and re-run the schema by
hand (psql -f schema.sql) — do NOT recreate the volume.
Runbook: ~/.claude/skills/infra-ops/references/db-runbook.md`,
	},
	{
		name: "volume-prune",
		fire: func(s, _ string) bool {
			return reVolumePrune.MatchString(s)
		},
		msg: `docker volume prune deletes EVERY volume not attached to a running
container — including database volumes whose stack merely happens to be stopped.
It cannot tell a stale cache from prod data, and -f skips the confirmation.

Reclaim space deliberately instead:
  docker volume ls -f dangling=true          # look FIRST
  docker volume rm <one-specific-non-db-volume>
  docker image prune -a                      # images are re-pullable; volumes are not`,
	},
	{
		name: "keychain-secret-dump",
		fire: func(s, _ string) bool {
			if reSecurityFind.MatchString(s) && reSecretValFlag.MatchString(s) {
				return true
			}
			return reDumpKeychain.MatchString(s) && reDumpDecrypt.MatchString(s)
		},
		msg: `Echoing a keychain secret's VALUE is banned — anything printed here lands
permanently in this session's on-disk transcript (this exact probe leaked a
vkp_ vitrinka token on 2026-08-27 and forced a rotation).

Secrets are used by REFERENCE, never by value — through the Onyx vault MCP:
  • discover:   secret_list (refs + notes, no values)
  • use:        run_command / http_call with env_refs / auth_ref injection
  • check:      secret_compare ("does this match X?") · secret_identify
  • store new:  secret_capture (human pastes into the app) · run_command capture

Metadata-only keychain reads (find-*-password without -w/-g, dump-keychain
without -d) are allowed and do not trip this guard.`,
	},
	{
		name: "system-prune-volumes",
		fire: func(s, _ string) bool {
			return reSystemPrune.MatchString(s) && reVolumesFlag.MatchString(s)
		},
		msg: `docker system prune --volumes deletes every unattached volume — same
data-loss risk as ` + "`docker volume prune`" + `, buried in a housekeeping command.

Drop the --volumes flag: ` + "`docker system prune -a`" + ` reclaims images, containers and
build cache, all of which are reproducible. Volumes are not.`,
	},
}

// destructiveMatch returns the first matching rule, or nil. Pure — unit-testable.
func destructiveMatch(cmd, cwd string) *destructiveRule {
	if cmd == "" || escapeHatch(cmd, "CLAUDE_ALLOW_DANGEROUS") {
		return nil
	}
	// Fast path: none of the rule keywords appear anywhere in the command →
	// zero regex work. This is ~95% of real calls.
	if !strings.Contains(cmd, "stash") && !strings.Contains(cmd, "checkout") &&
		!strings.Contains(cmd, "switch") && !strings.Contains(cmd, "restore") &&
		!strings.Contains(cmd, "docker") && !strings.Contains(cmd, "security") {
		return nil
	}
	for _, seg := range segments(cmd) {
		if textOnly(seg) {
			continue
		}
		for i := range destructiveRules {
			if destructiveRules[i].fire(seg, cwd) {
				return &destructiveRules[i]
			}
		}
	}
	return nil
}

func guardDestructive(in *HookInput) *Denial {
	if r := destructiveMatch(in.ToolInput.Command, in.CWD); r != nil {
		return deny("destructive:"+r.name, r.msg, destructiveEscape)
	}
	return nil
}
