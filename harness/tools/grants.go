// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

// BuiltinsFor resolves an agent's builtin declaration into its registry. Nil
// preserves the default of all builtins; an explicit empty slice grants none.
// Declarations accept concrete names and families: read (read_file, grep, glob),
// write (write_file, edit_file), and exec (bash). Unknown names grant nothing.
// Delegation is controlled separately by the runner's topology and depth.
func BuiltinsFor(grants []string) *Registry {
	r := Builtins()
	if grants == nil {
		return r
	}
	var names []string
	for _, grant := range grants {
		switch grant {
		case "read":
			names = append(names, "read_file", "grep", "glob")
		case "write":
			names = append(names, "write_file", "edit_file")
		case "exec":
			names = append(names, "bash")
		default:
			names = append(names, grant)
		}
	}
	return r.Select(names)
}
