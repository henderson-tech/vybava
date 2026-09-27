package claudeguards

import "testing"

// The shapes from the 2026-09-25 Reservine transcripts are refused; the
// sanctioned devbox run form and plain infra containers over ssh are not.
func TestDevboxSSHExec(t *testing.T) {
	for cmd, want := range map[string]bool{
		`ssh devops "docker exec -u 1000:1000 -e HOME=/tmp -w /workspace devbox-reservine-reservine-feat-x-e2e-playwright npx playwright test"`: true,
		`ssh devops docker exec -i -u sail devbox-reservine-reservine-vt-1003-e2e-app php artisan tinker`:                                       true,
		`ssh devulinka-wgadmin 'docker compose -p devbox-rsvb-templates exec -T laravel.test php artisan migrate'`:                              true,
		`ssh devulinka-wgadmin 'sudo -u devbox bash -c "cd /opt/devbox/rsvb && docker compose -p devbox-rsvb-templates exec -T app ls"'`:        true,
		`ssh devops '~/release-1002/slot -- docker exec devbox-reservine-reservine-vt-1002-e2e-playwright ls'`:                                  true,
		`devbox run --project reservine-be -- 'docker compose exec -T app php artisan migrate'`:                                                 false,
		`ssh devops 'docker exec eve-postgres-1 psql -c "select 1"'`:                                                                            false,
		`ssh devops 'docker logs devbox-reservine-x-app --tail 50'`:                                                                             false,
		`ssh devops 'docker exec infra-db-1 ls; ls /srv/devbox-state'`:                                                                          false,
		`docker exec devbox-local-app ls`: false,
	} {
		if got := devboxSSHExec(cmd); got != want {
			t.Errorf("%s: denied=%v, want %v", cmd, got, want)
		}
	}
}
