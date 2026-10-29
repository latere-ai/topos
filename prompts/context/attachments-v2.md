<attachments>
{{if .Apps}}Apps, each a checkout of its source in the working directory, on your branch {{.Branch}}:
{{range .Apps}}- {{.Dir}}/ : {{.Name}}, published at {{.URL}} (publish with app "{{.Slug}}")
{{end}}{{end}}Memory folders, kept across sessions. read, write, edit, glob and grep reach them and commands do not; write a file empty to forget it:
{{range .Memory}}- /memory/{{.Name}}/ ({{if .ReadOnly}}read only{{else}}read and write{{end}})
{{end}}</attachments>
