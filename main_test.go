package main

import "testing"

// TestHint pins what may reach the screen from a command line: a name, never
// a secret. Each secret case is a command line that once leaked.
func TestHint(t *testing.T) {
	cases := []struct{ prog, argv, want string }{
		// Secrets stay off screen.
		{"bash", "/bin/bash -lc ghp_FAKE_shorttok", ""},
		{"zsh", "zsh -ec hunter2", ""},
		{"python3", "python3 -Ic secretword123", ""},
		{"node", "node --eval sk_test_FAKEFAKE1234", ""},
		{"node", "node -p hunter2hunter2", ""},
		{"bb", "bb -e AKIAIOSFODNN7EXAMPLE", ""},
		{"perl", "perl -e alarm", ""},
		{"bb", "bb admin:hunter2@db.internal.example", ""},
		{"bb", "bb sk-live-xxxxxxxxxxxxxxx", ""},
		{"node", "node s3cr3tP4ssw0rdXyz12", ""},
		{"bb", "bb Hunter2Pass", ""},
		{"bb", "bb --password hunter2 run", ""},
		{"python3", "python3 -W ignore", ""},
		{"java", "java --source 11", ""},
		{"java", "java -Dpassword=x -cp a.jar Main", ""},
		{"sh", "sh -c bb dev", ""},
		// Names still show.
		{"bb", "bb test test/co/x_test.clj", "test"},
		{"bb", "bb dev_:inner", "dev_:inner"},
		{"bb", "bb lint:gate", "lint:gate"},
		{"java", "java -Xmx1g -cp a:b clojure.main -m kaocha.runner x", "kaocha.runner"},
		{"java", "java -jar /opt/app/app.jar --password hunter2", "app.jar"},
		{"node", "node --max-old-space-size=4096 server.js", "server.js"},
		{"node", "node --token=abc script.js", "script.js"},
		{"python3", "python3 -m http.server 8000", "http.server"},
		{"Python", "Python -u /x/y/serve.py", "serve.py"},
	}
	for _, c := range cases {
		if got := hint(c.prog, c.argv); got != c.want {
			t.Errorf("hint(%q, %q) = %q, want %q", c.prog, c.argv, got, c.want)
		}
	}
}

func TestSeconds(t *testing.T) {
	for etime, want := range map[string]int{"00:09": 9, "03:38": 218, "14:54:00": 53640, "2-01:00:05": 176405} {
		if got := seconds(etime); got != want {
			t.Errorf("seconds(%q) = %d, want %d", etime, got, want)
		}
	}
}
