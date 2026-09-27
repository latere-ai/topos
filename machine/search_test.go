// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var t0 = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

func tree() fstest.MapFS {
	f := func(s string, age int) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(s), ModTime: t0.Add(-time.Duration(age) * time.Hour)}
	}
	return fstest.MapFS{
		".gitignore":          f("build/\n*.log\n!keep.log\n# comment\n\n/rootonly.txt\n\\#hash.txt\n", 9),
		"main.go":             f("package main\n\nfunc main() {\n\tprintln(\"Hello\")\n}\n", 1),
		"util.go":             f("package main\n\nfunc helper() {}\n", 3),
		"keep.log":            f("hello from a kept log\n", 5),
		"drop.log":            f("hello from a dropped log\n", 5),
		"rootonly.txt":        f("hello\n", 5),
		"#hash.txt":           f("hello\n", 5),
		"build/out.go":        f("package build // hello\n", 5),
		".git/config":         f("hello\n", 5),
		"bin/tool":            f("hello\x00binary", 5),
		"pkg/lib.go":          f("package pkg\n// Hello there\nfunc Lib() {}\n", 2),
		"pkg/rootonly.txt":    f("hello\n", 5),
		"pkg/.gitignore":      f("gen/\n", 9),
		"pkg/gen/gen.go":      f("package gen // hello\n", 5),
		"docs/a/b/deep.md":    f("# Hello\n", 4),
		"docs/a/b/deeper.txt": f("nothing\n", 6),
	}
}

func lines(t *testing.T, q SearchRequest, rel string) SearchResult {
	t.Helper()
	res, err := SearchFS(t.Context(), tree(), "/work", rel, q)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGrepFilesHonorsGitignore(t *testing.T) {
	got := lines(t, SearchRequest{Kind: SearchGrep, Pattern: "(?i)hello"}, ".").Lines
	want := []string{"/work/docs/a/b/deep.md", "/work/keep.log", "/work/main.go", "/work/pkg/lib.go", "/work/pkg/rootonly.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("grep files:\n%v\nwant\n%v", got, want)
	}
}

func TestGrepBelowTheRootHonorsParentIgnores(t *testing.T) {
	got := lines(t, SearchRequest{Kind: SearchGrep, Pattern: "hello", CaseInsensitive: true}, "pkg").Lines
	want := []string{"/work/pkg/lib.go", "/work/pkg/rootonly.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("grep under pkg: %v", got)
	}
	got = lines(t, SearchRequest{Kind: SearchGrep, Pattern: "Hello", Glob: "*.go"}, "pkg").Lines
	if !slices.Equal(got, []string{"/work/pkg/lib.go"}) {
		t.Fatalf("grep under pkg with a glob: %v", got)
	}
}

func TestGrepModes(t *testing.T) {
	src := fstest.MapFS{"a.txt": {Data: []byte("one\ntwo match\nthree\nfour\nfive\nsix match\nseven\n")}}
	run := func(q SearchRequest) []string {
		q.Kind = SearchGrep
		res, err := SearchFS(t.Context(), src, "/w", ".", q)
		if err != nil {
			t.Fatal(err)
		}
		return res.Lines
	}
	if got := run(SearchRequest{Pattern: "match", OutputMode: ModeCount}); !slices.Equal(got, []string{"/w/a.txt:2"}) {
		t.Fatalf("count %v", got)
	}
	want := []string{"/w/a.txt-1-one", "/w/a.txt:2:two match", "/w/a.txt-3-three", "--", "/w/a.txt-5-five", "/w/a.txt:6:six match", "/w/a.txt-7-seven"}
	if got := run(SearchRequest{Pattern: "match", OutputMode: ModeContent, Context: 1}); !slices.Equal(got, want) {
		t.Fatalf("content with context:\n%s", strings.Join(got, "\n"))
	}
	if got := run(SearchRequest{Pattern: "two.*three", OutputMode: ModeContent, Multiline: true}); !slices.Equal(got, []string{"/w/a.txt:2:two match", "/w/a.txt:3:three"}) {
		t.Fatalf("multiline %v", got)
	}
	if got := run(SearchRequest{Pattern: "two.*three", OutputMode: ModeCount, Multiline: true}); !slices.Equal(got, []string{"/w/a.txt:1"}) {
		t.Fatalf("multiline count %v", got)
	}
	if got := run(SearchRequest{Pattern: "absent", OutputMode: ModeContent}); len(got) != 0 {
		t.Fatalf("no match %v", got)
	}
	if got := run(SearchRequest{Pattern: "MATCH", OutputMode: ModeCount, CaseInsensitive: true}); !slices.Equal(got, []string{"/w/a.txt:2"}) {
		t.Fatalf("case insensitive %v", got)
	}
	res, err := SearchFS(t.Context(), src, "/w", ".", SearchRequest{Kind: SearchGrep, Pattern: ".", OutputMode: ModeContent, HeadLimit: 3})
	if err != nil || len(res.Lines) != 3 || !res.Truncated {
		t.Fatalf("head limit %v %v", res, err)
	}
}

func TestGlobNewestFirst(t *testing.T) {
	got := lines(t, SearchRequest{Kind: SearchGlob, Pattern: "**/*.go"}, ".").Lines
	want := []string{"/work/main.go", "/work/pkg/lib.go", "/work/util.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("glob %v", got)
	}
	if got := lines(t, SearchRequest{Kind: SearchGlob, Pattern: "docs/**/*"}, ".").Lines; !slices.Equal(got, []string{"/work/docs/a/b/deep.md", "/work/docs/a/b/deeper.txt"}) {
		t.Fatalf("glob under docs %v", got)
	}
	if got := lines(t, SearchRequest{Kind: SearchGlob, Pattern: "*.go"}, "pkg").Lines; !slices.Equal(got, []string{"/work/pkg/lib.go"}) {
		t.Fatalf("glob from pkg %v", got)
	}
	big := fstest.MapFS{}
	for i := range GlobLimit + 5 {
		big[strings.Repeat("d/", i%3)+"f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String()+".txt"] = &fstest.MapFile{Data: []byte("x"), ModTime: t0}
	}
	res, err := SearchFS(t.Context(), big, "/b", ".", SearchRequest{Kind: SearchGlob, Pattern: "**/*.txt"})
	if err != nil || len(res.Lines) != GlobLimit || !res.Truncated {
		t.Fatalf("glob limit: %d lines, truncated %v, %v", len(res.Lines), res.Truncated, err)
	}
}

func TestSearchRefusesBadRequests(t *testing.T) {
	for name, q := range map[string]SearchRequest{
		"kind":       {Kind: "find", Pattern: "x"},
		"regexp":     {Kind: SearchGrep, Pattern: "("},
		"mode":       {Kind: SearchGrep, Pattern: "x", OutputMode: "lines"},
		"glob empty": {Kind: SearchGlob},
		"glob bad":   {Kind: SearchGlob, Pattern: "[a"},
	} {
		if _, err := SearchFS(t.Context(), tree(), "/work", ".", q); err == nil {
			t.Fatalf("%s: searched", name)
		}
	}
	if _, err := SearchFS(t.Context(), tree(), "/work", "missing", SearchRequest{Kind: SearchGrep, Pattern: "x"}); err == nil {
		t.Fatal("searched a directory that does not exist")
	}
}

func TestMatchGlob(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"a/**/c.go", "a/c.go", true},
		{"a/**/c.go", "a/x/y/c.go", true},
		{"a/**", "a/x/y", true},
		{"*.go", "a/b.go", false},
		{"a/*.go", "a/b/c.go", false},
		{"[", "x", false},
	} {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Fatalf("matchGlob(%q, %q) = %v", c.pattern, c.name, got)
		}
	}
}

// TestTheDenyListIgnoresCase: a case-insensitive filesystem opens every
// spelling below as the file an entry names, so each is refused: every
// home entry, the data directory's credentials, each base-name glob and
// the .env rule, with the home and data directories themselves spelled
// in another case, and with the Kelvin sign and the long s, which fold
// to k and s. The allowed .env files stay allowed in any case, and a
// name that is not an entry in any case stays readable.
func TestTheDenyListIgnoresCase(t *testing.T) {
	d := DenyList{Home: "/Users/Ada", DataDir: "/Users/Ada/Library/Topos"}
	for _, p := range []string{
		"/USERS/ADA/.SSH/known_hosts", "/users/ada/.Ssh", "/Users/Ada/.AWS/Credentials", "/Users/Ada/.Azure/x",
		"/Users/Ada/.CONFIG/GCLOUD/adc.json", "/Users/Ada/.KUBE/CONFIG", "/Users/Ada/.Docker/Config.JSON",
		"/Users/Ada/.NETRC", "/Users/Ada/.Git-Credentials", "/Users/Ada/.NPMRC", "/Users/Ada/.PyPIRC",
		"/Users/Ada/.GnuPG/pubring.kbx", "/Users/Ada/.Password-Store/x.gpg", "/Users/Ada/.Config/GH/Hosts.YML",
		"/users/ada/library/topos/CREDENTIALS/k",
		"/work/.ENV", "/work/.Env.Production", "/work/certs/SERVER.PEM", "/work/tls.Key", "/work/a.P12", "/work/b.PFX",
		"/work/ID_RSA", "/work/Id_Ecdsa.pub", "/work/ID_ED25519",
		"/work/tls.\u212aey", "/Users/Ada/.\u017f\u017fh/known_hosts",
	} {
		if !d.Path(p) {
			t.Errorf("%s is not denied", p)
		}
	}
	for _, p := range []string{"/Users/Ada/.SSHX", "/Users/Ada/.KUBE/CACHE", "/work/.ENV.EXAMPLE", "/work/.Env.Sample", "/work/.ENV.Template", "/work/KEYS.GO"} {
		if d.Path(p) {
			t.Errorf("%s is denied", p)
		}
	}
}

func TestDenyList(t *testing.T) {
	d := DenyList{Home: "/home/ada", DataDir: "/home/ada/.local/share/topos"}
	for _, p := range []string{
		"/home/ada/.ssh/id_ed25519", "/home/ada/.ssh", "/home/ada/.aws/credentials", "/home/ada/.kube/config",
		"/home/ada/.config/gh/hosts.yml", "/home/ada/.netrc", "/home/ada/.local/share/topos/credentials/k",
		"/work/.env", "/work/.env.production", "/work/certs/server.pem", "/work/tls.key", "/work/a.p12", "/work/id_rsa.pub",
	} {
		if !d.Path(p) {
			t.Fatalf("%s is not denied", p)
		}
	}
	for _, p := range []string{"/home/ada/.sshx", "/home/ada/.kube/cache", "/work/.env.example", "/work/.env.sample", "/work/.env.template", "/work/main.go", "/work/keys.go"} {
		if d.Path(p) {
			t.Fatalf("%s is denied", p)
		}
	}
	if (DenyList{}).Path("/home/ada/.ssh/id_rsa") != true {
		t.Fatal("an id_rsa file is readable when the home is unknown")
	}
	for name, want := range map[string]bool{
		"GITHUB_TOKEN": true, "db_password": true, "OPENAI_API_KEY": true, "SIGNING_PRIVATE_KEY": true,
		"AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true, "TOPOS_MODELS_KEY": true, "TOPOS_TOKEN": true,
		"TOPOS_LOCAL_ISSUER_KEY": true, "TOPOS_CREDENTIALS_KEY": true, "TOPOS_DB_URL": true, "topos_blob_secret_key": true,
		"PATH": false, "HOME": false, "TOKENIZER": false, "AWS_REGION": false,
	} {
		if EnvDenied(name) != want {
			t.Fatalf("EnvDenied(%s) = %v", name, !want)
		}
	}
	got := FilterEnv([]string{"PATH=/bin", "GITHUB_TOKEN=x", "HOME=/home/ada", "NOVALUE"})
	if !slices.Equal(got, []string{"PATH=/bin", "HOME=/home/ada", "NOVALUE"}) {
		t.Fatalf("FilterEnv %v", got)
	}
}
