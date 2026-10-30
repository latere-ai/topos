<attachments>
{{if .Apps}}Apps, each a checkout of its source in the working directory, on your branch {{.Branch}}:
{{range .Apps}}- {{.Dir}}/ : {{.Name}}, published at {{.URL}} (publish with app "{{.Slug}}")
{{end}}{{end}}Memory folders, kept across sessions. read, write, edit, glob and grep reach them, and so does a command that uses only ls, cat, head, tail, wc, grep, find, sort, uniq, cut, mkdir, rm, mv, cp, touch and the shell's builtins; any other program does not see them. Write a file empty to forget it:
{{range .Memory}}- /memory/{{.Name}}/ ({{if .ReadOnly}}read only{{else}}read and write{{end}})
{{end}}</attachments>
