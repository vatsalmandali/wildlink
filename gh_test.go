package main

import (
	"strings"
	"testing"
)

func TestGithubAPI(t *testing.T) {
	want := "https://api.github.com/repos/vatsal/wildlink/releases/latest"
	for _, in := range []string{
		"vatsal/wildlink", "github:vatsal/wildlink", "https://github.com/vatsal/wildlink",
		"github.com/vatsal/wildlink/", "https://github.com/vatsal/wildlink/releases",
	} {
		if got := githubAPI(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	for _, in := range []string{"https://example.com/update.json", `\\SERVER\Share\update.json`, "/tmp/update.json"} {
		if got := githubAPI(in); got != "" {
			t.Errorf("%q should not be a repo, got %q", in, got)
		}
	}
}

func TestParseGitHubRelease(t *testing.T) {
	raw := []byte(`{"tag_name":"v2.3.0","name":"Wild Link 2.3.0","draft":false,
	 "body":"Faster file transfers\r\n\r\n- resume","assets":[
	  {"name":"WildLink-Setup.exe","browser_download_url":"https://github.com/a/b/releases/download/v2.3.0/WildLink-Setup.exe"},
	  {"name":"WildLink-mac.zip","browser_download_url":"https://github.com/a/b/releases/download/v2.3.0/WildLink-mac.zip"}]}`)
	u := parseGitHubRelease(raw, "windows")
	if u == nil || u.Version != "2.3.0" {
		t.Fatalf("got %+v", u)
	}
	if u.Notes != "Faster file transfers" {
		t.Errorf("notes: %q", u.Notes)
	}
	if u.File == "" || !strings.HasSuffix(u.File, "WildLink-Setup.exe") {
		t.Errorf("windows installer not picked: %q", u.File)
	}
	if mac := parseGitHubRelease(raw, "darwin"); mac == nil || !strings.HasSuffix(mac.File, "WildLink-mac.zip") {
		t.Errorf("mac download not picked")
	}
	if !newerVersion(u.Version, appVersion) {
		t.Errorf("2.3.0 should be newer than %s", appVersion)
	}
	if parseGitHubRelease([]byte(`{"tag_name":"v9","draft":true}`), "windows") != nil {
		t.Errorf("drafts must be ignored")
	}
}
