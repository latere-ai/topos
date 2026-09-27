The conversation so far:
{{range .Entries}}
{{if eq .Kind "text"}}{{if .Agent}}Agent{{else}}Person{{end}}: {{.Text}}{{else if eq .Kind "call"}}Agent called {{.Name}} with {{.Args}}{{else}}The call returned:
{{.Text}}{{if .Cut}}
[cut]{{end}}{{end}}
{{end}}
