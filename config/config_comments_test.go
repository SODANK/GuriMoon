package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAcceptsComments(t *testing.T) {
	src := `{
  // line comment
  "database": {"host": "h", "user": "u", "db": "d"}, // trailing
  /* block
     comment */
  "esp": {"clientPath": "http://not//a/comment", "newsListen": "a\"//b"}
}`
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ESP.ClientPath != "http://not//a/comment" {
		t.Errorf("clientPath = %q", cfg.ESP.ClientPath)
	}
	if cfg.ESP.NewsListen != `a"//b` {
		t.Errorf("newsListen = %q", cfg.ESP.NewsListen)
	}
}

func TestExampleConfigsLoad(t *testing.T) {
	for _, f := range []string{"../config.example.json", "../config.docker.json"} {
		if _, err := Load(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
