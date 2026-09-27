// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// homeDenied are the credential paths under the home directory (spec
// 009). An entry ending in "/" is a directory and everything in it.
// Every entry here and in nameDenied and envAllowed is written in lower
// case, the form Path compares in.
var homeDenied = []string{
	".ssh/", ".aws/", ".azure/", ".config/gcloud/", ".kube/config",
	".docker/config.json", ".netrc", ".git-credentials", ".npmrc", ".pypirc",
	".gnupg/", ".password-store/", ".config/gh/hosts.yml",
}

// nameDenied are the base-name patterns refused in any root.
var nameDenied = []string{"*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ecdsa*", "id_ed25519*"}

// envAllowed are the .env files that hold no secrets by convention.
var envAllowed = []string{".env.example", ".env.sample", ".env.template"}

// DenyList is the credential deny-list of one machine: the person's home
// directory and the data directory whose credentials/ it names.
type DenyList struct {
	Home    string
	DataDir string
}

// Path reports whether the file tools refuse p, an absolute clean path.
// Every entry matches without regard to case, on every platform: APFS
// and NTFS, the default filesystems of macOS and Windows, open
// ~/.SSH/ID_RSA or SERVER.PEM as the file the entry names, so a match
// that heeded case would let a differently cased path read it. On a
// case-sensitive filesystem, as on most Linux hosts, this also refuses
// a file whose name differs from an entry only in case, such as
// KEY.PEM, which is an accepted over-denial.
func (d DenyList) Path(p string) bool {
	p = fold(filepath.Clean(p))
	if d.Home != "" {
		home := fold(d.Home)
		for _, e := range homeDenied {
			if within(p, filepath.Join(home, e), strings.HasSuffix(e, "/")) {
				return true
			}
		}
	}
	if d.DataDir != "" && within(p, filepath.Join(fold(d.DataDir), "credentials"), true) {
		return true
	}
	base := filepath.Base(p)
	if base == ".env" || (strings.HasPrefix(base, ".env.") && !slices.Contains(envAllowed, base)) {
		return true
	}
	for _, pat := range nameDenied {
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// fold maps s to one case, so two spellings a case-insensitive
// filesystem opens as one file compare equal. Each rune goes to its
// upper case and then to that one's lower case, which also maps the
// Kelvin sign to k and the long s to s, as the Unicode case folding of
// those filesystems does.
func fold(s string) string {
	return strings.Map(func(r rune) rune { return unicode.ToLower(unicode.ToUpper(r)) }, s)
}

// within reports whether p is target, or under it when dir is set.
func within(p, target string, dir bool) bool {
	target = filepath.Clean(target)
	if p == target {
		return true
	}
	return dir && strings.HasPrefix(p, target+string(filepath.Separator))
}

// envSuffixes are the variable-name endings that hold secrets.
var envSuffixes = []string{"_TOKEN", "_SECRET", "_PASSWORD", "_API_KEY", "_PRIVATE_KEY"}

// envNames are the variables refused by name.
var envNames = []string{"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}

// envPrefix is Topos's own configuration, refused whole: a server's
// holds its signing key, its credentials key and its database URL,
// whose names no suffix catches.
const envPrefix = "TOPOS_"

// EnvDenied reports whether a variable is kept from commands.
func EnvDenied(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, envPrefix) || slices.Contains(envNames, upper) {
		return true
	}
	for _, s := range envSuffixes {
		if strings.HasSuffix(upper, s) {
			return true
		}
	}
	return false
}

// FilterEnv returns environ, "NAME=value" entries, without the denied
// variables.
func FilterEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !EnvDenied(name) {
			out = append(out, kv)
		}
	}
	return out
}
