package memstore

import "testing"

func scopeEnv() Env {
	return Env{
		Home:         "/home/admin",
		Ports:        []int{9111, 8096},
		HostNames:    []string{"aiserver.tail878d9e.ts.net"},
		HostIPs:      func() []string { return []string{"100.96.103.31", "192.168.0.75"} },
		OtherHosts:   []string{"MediaServer", "docker-server"},
		Exists:       func(p string) bool { return false }, // nothing exists: only foreign refs may escape failure
		SystemctlCat: func(string) (bool, bool) { return false, true },
		Listening:    func(int) bool { return false },
	}
}

func TestOtherMachinesAreUnverifiableNotFailed(t *testing.T) {
	cases := map[string]string{
		"ssh root@mediaserver 'ls /mnt/storage/backups/homelab'":         "ssh context",
		"pct exec 200 -- cat /opt/docker/docker-compose.yml":             "pct exec",
		"scp build@10.0.0.7:/srv/out/app.tar ~/":                         "user@host",
		"copy mediaserver:/mnt/storage/media/x.mkv here":                 "host:/path",
		"on MediaServer edit /opt/docker/nginx-proxy/nginx.conf":         "named other host",
		"on 100.104.116.16 check /var/log/syslog":                        "other IP",
		"on docker-server.tail878d9e.ts.net read /etc/hosts":             "other ts.net name",
		"restart jellyfin.service on mediaserver.tail878d9e.ts.net":      "unit with other host",
		"ssh root@100.104.116.16 curl localhost:8096/health (port 8096)": "port in ssh context",
		"read /home/someoneelse/projects/x/README.md":                    "other user's home",
	}
	for text, why := range cases {
		all, failed := RunChecks(text, Checkers(scopeEnv()))
		if len(failed) != 0 {
			t.Errorf("%s: %q must not fail: %v", why, text, failed)
		}
		if len(all) == 0 || Verifiable(all) != 0 {
			t.Errorf("%s: %q should be reported unverifiable, got %+v", why, text, all)
		}
		if n := UnverifiableNote(all); n == "" {
			t.Errorf("%s: no note", why)
		}
	}
}

func TestThisMachinesReferencesStillFail(t *testing.T) {
	env := scopeEnv()
	text := "1. run /opt/grimoire/deploy.sh\n2. systemctl restart grimoire.service\n3. curl 127.0.0.1:9111/api/health (port 9111)\n4. on this host, also read 100.96.103.31:9111"
	all, failed := RunChecks(text, Checkers(env))
	if len(failed) != 3 || Verifiable(all) != 3 {
		t.Fatalf("all=%+v failed=%+v", all, failed)
	}
}

func TestMixedProcedureChecksOnlyWhatIsHere(t *testing.T) {
	text := "run /opt/grimoire/deploy.sh\nssh root@mediaserver 'systemctl restart foo.service; ls /opt/docker/x'"
	all, failed := RunChecks(text, Checkers(scopeEnv()))
	if len(failed) != 1 || failed[0].Subject != "/opt/grimoire/deploy.sh" {
		t.Fatalf("failed=%+v", failed)
	}
	unv := 0
	for _, r := range all {
		if r.Unverifiable {
			unv++
		}
	}
	if unv != 2 {
		t.Errorf("want the unit and the remote path unverifiable, got %+v", all)
	}
}
