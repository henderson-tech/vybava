package claudeguards

import (
	"regexp"
	"slices"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// ---------------------------------------------------------------------------
// machine:devbox-ssh-exec - `ssh <host> … docker (compose) exec` into a Devbox
// workspace container, around the devbox CLI. Its containers and compose
// projects are all named `devbox-…`; reaching them over raw ssh skips the
// admission, holds and workspace resolution `devbox run` does, and was the
// habit in 190 Reservine calls by 2026-09-25 (`ssh devops "docker exec
// devbox-reservine-…-e2e-playwright …"`, `docker compose -p devbox-rsvb-…
// exec`). Only a `devbox-` target is refused: plain infra containers over ssh
// stay the infra repos' business.
// ---------------------------------------------------------------------------

// reDockerExecCall is one docker invocation up to the next separator, so the
// `exec` and the `devbox-` target are read from the same remote command.
var reDockerExecCall = regexp.MustCompile(`\bdocker(?:[ \t]+compose|-compose)?[ \t][^;&|\n]*`)

func devboxSSHExec(cmd string) bool {
	for _, seg := range shellseg.Segments(cmd) {
		if shellseg.CommandWord(seg) != "ssh" {
			continue
		}
		for _, call := range reDockerExecCall.FindAllString(seg, -1) {
			if strings.Contains(call, "devbox-") && slices.Contains(strings.Fields(call), "exec") {
				return true
			}
		}
	}
	return false
}

func guardDevboxSSHExec(in *HookInput) *Denial {
	if !devboxSSHExec(in.ToolInput.Command) {
		return nil
	}
	return deny("machine:devbox-ssh-exec", `This reaches a Devbox workspace container over raw ssh. The devbox CLI owns
those containers (admission, holds, which workspace is this branch's), so go
through it from the branch's checkout:
    devbox run --project <p> -- 'docker compose exec -T <svc> <command>'
<svc> is the compose service (app, db, playwright, laravel.test); --project is
needed only when the checkout has no devbox.yaml, and 'devbox status --json'
names the workspace. Plain infra containers over ssh are not affected.`, "")
}
